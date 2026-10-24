# Deployment: one compose, three scenarios

**Date:** 2026-09-30 · Current for tags `r83-submodule-v24+`
**Images:** `ollama-legion/balancer:r83-submodule-v24+`,
`ollama-legion/agent:r83-submodule-v24+`,
`ollama-legion/webui:r83-submodule-v24+`,
`ollama-legion/cppworker:gpu-r83-submodule-v25+` (capacity/parallelism —
see §7 and 7.2; `cppworker v25` is required so that `/api/models` honestly returns
`max_slots`).

> **The canonical file is `deployments/docker-compose.stack.yml`.** The folder also
> holds 18 more compose files (historical and highly specialized: RPC, cocoindex,
> test-stub, fanout stand). New stacks need only this one; the rest are kept
> for reproducing old runs and are marked at the end of the document.

---

## 1. Three scenarios — choosing a profile

| Scenario | Profile | What comes up | When it is needed |
|---|---|---|---|
| **1. Everything on one machine** | `--profile full` | cppworker + imageworker + balancer + webui | one GPU machine, clients connect to it |
| **2. Backends only** | `--profile worker` | cppworker + imageworker | the balancer already runs on another machine |
| **3. Control plane only** | `--profile balancer` | balancer + webui | backends register themselves from other machines |
| 4. External agent (Ollama / rollback) | `--profile legacy-agent` | agent | an Ollama backend that has no code of ours, or a rollback to the external metrics collector |

> **R-Image follow-up (2026-10-07): where the metrics agent now lives.**
> The metrics collector for llama.cpp workers is embedded IN cppworker
> (`AGENT_EMBEDDED=on`, port 18034) and writes into the same backend record.
> That is why the `full` profile no longer has a separate `agent` container.
> The external agent remains for **Ollama** backends and only for them — it has
> been moved into a SEPARATE `legacy-agent` profile: Ollama has none of our Go
> code, so there is nobody to collect the metrics.
>
> Two collectors on one backend MUST NOT be installed: the metrics arrive in
> turn and the values "blink" — in particular `gpu.uuids`, by which the balancer
> understands that several backends share one physical GPU (otherwise the total
> VRAM doubles: 16 GB on an 8 GB card).

```bash
cd deployments
# first time:
cp .env.bundled-with-agent.example .env.bundled-with-agent
# in deployments/.env set a single stack token:
#   CPPWORKER_API_TOKEN=<your-token>

# scenario 1
docker compose -f docker-compose.stack.yml --profile full up -d
# scenario 2 (address of the external balancer — BALANCER_URL in .env!)
docker compose -f docker-compose.stack.yml --profile worker up -d
# scenario 3
docker compose -f docker-compose.stack.yml --profile balancer up -d
# scenario 4 (rollback): external agent instead of the embedded one
#   in .env: CPPWORKER_AGENT_EMBEDDED=off  and  BALANCER_URL=<external>
docker compose -f docker-compose.stack.yml --profile worker --profile legacy-agent up -d
```

To check which services fall into a profile **without starting** them:

```bash
docker compose -f docker-compose.stack.yml --profile full --services
# → cppworker-gpu, imageworker, loadbalancer, webui
docker compose -f docker-compose.stack.yml --profile worker --services
# → agent, cppworker-gpu, imageworker
```

**Scenario 2 requires BALANCER_URL.** Without it the workers go to
`http://loadbalancer:18081` — a service name that does not exist in this profile
(it lives on another machine) — and registration retries with an error forever.
It is set ONCE in `deployments/.env`:

```bash
BALANCER_URL=http://192.0.2.110:18081     # the machine with the balancer
CPPWORKER_API_TOKEN=<the same token as on the balancer>
```

The value travels to all three places: `CPPWORKER_BALANCER_URL` (the worker
itself), `SDWORKER_BALANCER_URL` (the image worker) and `BALANCER_URL` (the
embedded agent).

---

## 2. Scenario 1: everything on one machine

```bash
docker compose -f docker-compose.stack.yml --profile full up -d
docker ps --format '{{.Names}}\t{{.Status}}' | grep ol-stack
```

| Address | What it is |
|---|---|
| `http://<host>:18080` | client API (OpenAI + Ollama) — this is what OpenWebUI/Cline points at |
| `http://<host>:18081` | admin API (backend registration, metrics, WebUI) |
| `http://<host>:18083` | operator dashboard |
| `http://<host>:18092` | cppworker port (needed only inside the network, but exposed) |

**Check after start:**

```bash
TOKEN=$(grep '^CPPWORKER_API_TOKEN=' .env | cut -d= -f2)
curl -s -H "X-API-Token: $TOKEN" http://127.0.0.1:18081/api/v1/backends | jq '.backends[] | {id,host,cppWorkerPort,status,maxConcurrentRequests}'
```

**Exactly one** record is expected (`cppworker-gpu-bundled-agent`, `status=healthy`,
`maxConcurrentRequests` = your `AGENT_MAX_CONCURRENT_REQUESTS`).

Selecting and loading a model (exactly the way the client does it):

```bash
curl -s http://127.0.0.1:18080/api/tags | jq -r '.models[].name'       # what is available
curl -s -H "X-API-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"<name>","messages":[{"role":"user","content":"hi"}],"stream":false}' \
  http://127.0.0.1:18080/api/chat -D - | head -20
# → 200 and the X-Backend-Id header
```

**The first request to an unloaded model** takes as long as the load itself
(27B Q4 on an A10 — minutes). The balancer waits for it `LB_AUTO_LOAD_WAIT_SEC`
(in this compose the default is **900 s**); if it does not wait it out, it returns
`503` with `Retry-After`, and the client repeats the request. This is normal
behavior, not a failure.

### 2.1 The first 27B load and client timeouts (important for Cline)

Loading 27B on an A10 takes **3–8 minutes** (311 s was observed for
`Qwen3.8-27B-UD-Q4_K_M`). Three practical rules follow from this:

1. **The client must survive a long first request.** The balancer honestly waits up
   to `LB_AUTO_LOAD_WAIT_SEC`, but if the client's timeout is shorter (Cline by
   default drops the connection earlier), the client will see "it does not work",
   even though the balancer and cppworker are working. For the first request raise
   the client timeout or load the model in advance.
2. **Load the model in advance** — then the client request is served immediately:

   ```bash
   curl -s -H "X-API-Token: $TOKEN" -H 'Content-Type: application/json' \
     -d '{"name":"Qwen3.8-27B-UD-Q4_K_M","contextSize":32768}' \
     'http://127.0.0.1:18092/api/models/load-with-params?wait=false'
   # progress: /api/models/load/progress?model=Qwen3.8-27B-UD-Q4_K_M
   ```

   `wait=false` (or omitting `wait`) is mandatory: a synchronous wait with the
   default `waitTimeoutMs=60000` aborts the load before it finishes.
3. **Do not abort a load halfway.** If the load HTTP request is cancelled by a
   timeout, cppworker aborts the load ("load aborted via watcher"), the model
   stays in the `loading` state, and its slot is occupied — subsequent requests to
   that model will wait. It is cured by reloading the model (item 2) or restarting
   `cppworker-gpu`.

---

## 3. Scenario 2: backends only, balancer on another machine

This is the most frequent case behind "cppworker does not connect". **Two**
variables are needed, and both go into `.env.bundled-with-agent`:

```bash
# deployments/.env.bundled-with-agent
BALANCER_URL=http://<balancer-host>:18081     # ADMIN API, NOT 18080!
BACKEND_HOST=<address of THIS machine, visible FROM the balancer>
```

- `BALANCER_URL` is read by the **agent** (it registers the backend and sends
  metrics). Port **18081** is the admin API; registration does not work on 18080.
- `BACKEND_HOST` reaches the balancer as the backend address: it will proxy
  inference to `BACKEND_HOST:18092`. The compose service name (`cppworker-gpu`)
  will **not** do here — it resolves only inside this network. Options: the
  machine's LAN IP, a DNS name, `host.docker.internal` (if the balancer is on the
  same host).

```bash
docker compose -f docker-compose.stack.yml --profile worker up -d
```

Checking that the backend is visible **from the balancer**:

```bash
curl -H "X-API-Token: $TOKEN" http://<balancer-host>:18081/api/v1/backends
```

And that the balancer actually reaches inference:

```bash
curl -H "X-API-Token: $TOKEN" http://<balancer-host>:18092/health   # from the balancer machine
```

If the last command does not answer, the problem is in the network/firewall or in
`BACKEND_HOST`, not in cppworker.

---

## 4. Scenario 3: control plane only

```bash
docker compose -f docker-compose.stack.yml --profile balancer up -d
```

Backends on other machines register themselves (their
`.env.bundled-with-agent` must contain
`BALANCER_URL=http://<this-host>:18081`). Until they do,
`/api/v1/backends` is empty — that is normal.

---

## 4.1 Mixed mode: Ollama and llama.cpp at the same time

The balancer serves **both types of text backends simultaneously** — this is the
standard mode, not an experiment. They are separated by surfaces (ports), so the
client itself chooses which path to take, and nothing has to be configured in the
client.

| External port | Surface | What it accepts | Where it routes |
|---|---|---|---|
| `18079` | OpenAI | `/v1/chat/completions`, `/v1/models`, `/v1/images/*` | llama.cpp and image backends |
| `18080` | Ollama | `/api/chat`, `/api/generate`, `/api/tags`, `/api/ps` | Ollama and llama.cpp backends |
| `18081` | Admin | `/api/v1/*` (registration, metrics, WebUI) | — (no client surfaces) |

Verified on a live stand: `18079/v1/models` answers in the OpenAI format
(`{"data":[{"id":…,"object":"model"}]}`), `18080/api/tags` in the Ollama format,
and `18081/v1/models` returns 404: it is the admin API, clients do not go there.

**How the backend is chosen.** Each type has its own router: `OllamaRouter`
(`BackendType=ollama`) and `LlamaCppRouter` (`BackendType=llama_cpp`), while the
image path goes into `ImageRouter`. A router selects backends of ITS OWN type
(`isBackendTypeAllowed`), so a request to the Ollama surface will not "by mistake"
go to a llama.cpp worker, and vice versa. On top of that there is a deliberate
cross-fallback: if a model arrives on the Ollama surface that the Ollama backends
do not have, but llama.cpp has it loaded, the request goes to llama.cpp (see
`ollama_router.go`).

**What this gives in practice.** On one machine you can keep a "hot" llama.cpp
(GGUF, its own n_ctx, KV types) and next to it Ollama with its models — the client
goes to both through ONE address, and the balancer accounts for both pools.

### Bringing up both types

```bash
cd deployments

# 1. llama.cpp pool (cppworker + imageworker + balancer + webui)
docker compose -f docker-compose.stack.yml --profile full up -d

# 2. Ollama — on the same machine, on its own port (11434 by default).
#    It is registered by the AGENT: Ollama has none of our Go code, there is nobody to collect metrics.
#    For llama.cpp workers the agent is not needed — it is embedded there (AGENT_EMBEDDED=on).
docker run -d --name ollama --gpus all -p 11434:11434 ollama/ollama

# 3. The agent for Ollama. Two variables in deployments/.env:
#      AGENT_BACKEND_TYPE=ollama
#      OLLAMA_URL=http://host.docker.internal:11434
#    (note: in .env the name is AGENT_BACKEND_TYPE, but inside the container it
#     turns into BACKEND_TYPE — that is how the compose file is built; see §5)
docker compose -f docker-compose.stack.yml --profile legacy-agent up -d agent
```

> The external `agent` container is the **only** way to connect an Ollama backend:
> it registers it in the balancer and supplies metrics. For llama.cpp workers the
> agent is not needed — it is embedded there (`AGENT_EMBEDDED=on`).
>
> `AGENT_BACKEND_TYPE` defaults to `llama_cpp` — the previous behavior, so stands
> without Ollama notice nothing.

### Checking that both types are visible

```bash
# both types in the registry
curl -s localhost:18081/api/v1/backends | jq '.backends[] | {id, type}'
#   → {"id":"cppworker-gpu-bundled-agent","type":"llama_cpp"}
#     {"id":"ollama-1","type":"ollama"}

# mixed cluster: effectiveBackendType is EMPTY — this is NORMAL, not an error
curl -s localhost:18081/api/v1/cluster | jq '{effectiveBackendType, backendTypeCounts}'
#   → {"effectiveBackendType":"","backendTypeCounts":{"llama_cpp":1,"ollama":1}}

# which routers are involved (in the balancer log)
docker logs ol-stack-balancer 2>&1 | grep 'routeRequest: backend type resolved'
```

**Mixed-mode trap: `backendEngine`.** If `config/config.json` (the balancer) has
`backendEngine: ollama_api`, then `getEffectiveBackendType()` returns `ollama`,
and `filterBackendsByEffectiveType()` throws out ALL llama_cpp backends from the
output of `/api/v1/metrics` and `/api/ps`. Symptoms: the monitor is empty, Ollama
clients get 503 "no healthy backends". For a stack with cppworker the value must be
**`llama_cpp`**; with a genuine mix it stays `llama_cpp`, and `effectiveBackendType`
in `/api/v1/cluster` becomes empty — that is the sign of "we show all types".

---

## 4.2 Several machines: what adds up and what does not

This is the main question when adding a second/third machine, and the answer
differs for text and for images.

| What | Does the VRAM of several machines add up | How it is distributed |
|---|---|---|
| **Text** (llama.cpp) | **Yes**, if the model is split across machines | layer by layer via RPC (see below) or as independent replicas |
| **Images** (stable-diffusion.cpp) | **No** | ONE worker = ONE card: generation entirely on one machine |

**Why images do not add up.** `sd-server` is one process per model, `sd_ctx` is
created once before `listen()`, and `ggml` inside is patched, so the layers cannot
be spread over the network (see `docs/archive/research/sdcpp-lowvram-integration.ru.md`
§5.3). 24 GB "across three hosts" does **not** combine for image generation: each
generation runs entirely on the card of one worker. Three machines give three
independent pools, and the balancer distributes REQUESTS between them (and does not
let two generations occupy one card — the `coexistence` policy).

**What this means in practice.** An image model that needs more VRAM than one card
has cannot be deployed across the network. It is worked around with quantization
(a smaller GGUF) and offload (`--offload-to-cpu`, `--vae-tiling`).

### Distributing a TEXT model across the network: status in the project

| Mechanism | Files | Status |
|---|---|---|
| Separate llama.cpp workers on each machine, the balancer scatters requests | `cmd/cppworker`, `internal/balancer` | ✅ works (the standard `--profile worker` on each machine) |
| RPC coordinator + RPC workers (layer-by-layer pipeline) | `internal/rpccoordinator`, `internal/rpcworker` | ⚠️ scaffolding: the HTTP layer, health, split/merge, KV shards and 66 tests exist, but real inference is a **stub** (`stubMode`, "Real llama.cpp integration — B1.real"). Stack: `deployments/docker-compose.rpc.yml` |
| Tensor Parallelism (parts of matrices inside a layer, `rptensor`) | `internal/rptensor` | ⬜ not started: see `docs/plans/b8-tensor-parallelism-plan.md` — "NOT STARTED, 20–30 days" |
| `llama_rpc` / `ggml_backend_rpc` integration | — | ❌ absent from the code |

**A conclusion worth knowing BEFORE buying/connecting hardware.** Right now 24 GB
across three machines for ONE large text model cannot be assembled the standard
way — RPC inference in the project is still a stub, and native `--rpc` support in
llama.cpp has not been wired into the code. Two options actually work:

1. **Replicas**: the same model is loaded on each machine (whether it fits depends
   on each machine's VRAM), and the balancer distributes requests. Plus — linear
   throughput; minus — the model must fit on ONE card.
2. **Your own build with `--rpc`**: llama.cpp can do `rpc-server` on each machine
   plus `--rpc` on the client, and the layers travel over the network. This gives
   "one model on three cards", but it requires your own build/configuration and a
   **fast network**: activations are shuffled between nodes at every layer, so
   1 Gbit/s kills the gain (a guideline is 10 Gbit/s and above, or better
   InfiniBand).

---

## 4.3 Previous-generation cards (GTX 1070 and relatives)

The short answer: **for image generation — yes, it is suitable; for text — yes, but
with caveats about speed.**

**Why it is suitable.** Both builds of the project treat Pascal (`sm_61`) as
supported:

- `cppworker` (text) is built on **CUDA 12.2** (`docker/cppworker/Dockerfile.gpu`:
  `FROM ${CUDA_BASE}:12.2.0-devel-ubuntu22.04`). CUDA 12.x supports Pascal; it is
  enough to build with `CUDA_ARCH=61` (in `.env`/`build-containers.ps1`).
  Important: CUDA **13** dropped Pascal support — do not raise the toolkit version
  above 12.x.
- `imageworker` (images) is built on **Vulkan** (`SD_SERVER_ASSET=...-vulkan.zip`)
  and on Vulkan works independently of the CUDA architecture. The Vulkan ICD comes
  with the driver, so a 1070 is a working card for `sd-server`.

**Caveats worth taking into account:**

- **FP16 on Pascal is slow.** The `1070` has no fast fp16 (it is emulated), so
  SD models in fp16 can compute slower than the core count suggests. In practice
  this means "longer per image", not "does not work".
- **SDXL/FLUX on 8 GB** — by the same rules as on an RTX 3070: offload is needed
  (`--offload-to-cpu`), VAE tiling and a smaller quant. Guidelines are in
  `docs/archive/research/sdcpp-lowvram-quants.md`.
- **Different cards in one pool are normal.** The balancer distinguishes physical
  cards by UUID (`gpu.uuids`), so "3070 + 1070" will not be confused and the total
  VRAM in Monitor will not double. Each worker still computes on its own card;
  there is NO memory pooling between them (see §4.2).
- **The pool speed is set by the slowest card** in replica mode: a request goes to
  whichever machine has the model free, so the overall throughput adds up, but an
  individual request on a 1070 will be slower than on a 3070.

---

## 4.4 A second machine with workers: what to set and what to check

Verified on a live pair: the balancer on `192.0.2.10`, the workers on `192.0.2.11`.

### What to set in `deployments/.env` ON THE REMOTE MACHINE

```bash
BALANCER_URL=http://192.0.2.10:18081     # balancer ADMIN API (:18081, NOT :18080)
CPPWORKER_API_TOKEN=<the same token as on the balancer>
BACKEND_HOST=192.0.2.11                  # address of THIS machine, visible from the balancer

# Record IDs — MUST be unique between machines (see below about 409).
# On a worker host the text backend is registered by the AGENT, so its ID is
# AGENT_ID, not CPPWORKER_REGISTER_NAME (cppworker does not register itself:
# CPPWORKER_REGISTER_DISABLE=true).
AGENT_ID=cppworker-34
SDWORKER_BACKEND_ID=imageworker-34
```

`BACKEND_HOST` travels into four places at once: `SDWORKER_ADVERTISE_HOST` (the
address for `response_format:"url"` and for proxying), `AGENT_CPPWORKER_HOST` (the
address under which the balancer sees the text backend), `AGENT_PUBLIC_HOST` (the
agent's `/health` address) and `CPPWORKER_ADVERTISE_HOST`. Previously it was
substituted into only two of them, so even a successful registration produced an
unreachable address.

### ⚠️ The container name is NOT an address: check the HOST field of every backend

If `BACKEND_HOST` on a remote machine is not set (or the container was not
recreated after it was added), the worker registers under its container name —
`imageworker` / `cppworker-gpu`. Inside the balancer's docker network these names
resolve to **its own** containers, therefore:

* the remote machine's record looks alive and "local";
* the metrics (CPU/GPU/VRAM) honestly arrive from it — they are sent by its own
  agent;
* but **HTTP requests go to the first machine**, because the URL is built from the
  host name.

Check on the live pair: `/api/v1/image/backends/IMAGEWORKER-34/models` returned
exactly the same list of two models as the local `imageworker`, whereas the real
worker on the second machine had **0** models.

How to catch it:

```bash
# 1. In the WebUI the HOST field of every backend must be an address (192.0.2.11),
#    not a container name. In the API:
curl -s -H "X-API-Token: $TOKEN" http://localhost:18081/api/v1/backends \
  | jq '.backends[] | {id, host, nodeAddr}'

# 2. The balancer log must not contain a warning about an identical host:
docker logs ol-stack-balancer 2>&1 | grep 'advertise the SAME host'
#    registration guard: two different nodes advertise the SAME host —
#    host=imageworker backend=IMAGEWORKER-34 node=172.23.0.1
#    otherBackend=imageworker otherNode=172.23.0.5

# 3. On the remote machine — what the container actually sees:
docker exec <imageworker-container> env | grep -E 'BACKEND_HOST|ADVERTISE_HOST'
```

The cure is to set `BACKEND_HOST=192.0.2.11` in `deployments/.env` on the remote
machine and **recreate** the workers (editing `.env` alone does not touch the
container):

```bash
docker compose -f docker-compose.stack.yml --profile worker up -d --force-recreate cppworker-gpu imageworker
```

### What is downloaded to a remote worker and what is not (WebUI)

| Surface | How it is addressed | Where the file lands |
|---|---|---|
| **Images** (Image page, `POST /api/hf/bundle`) | through the balancer: `/api/v1/image/backends/{id}/...` | on the selected image worker |
| **Text/GGUF** (HuggingFace tab, `POST /api/hf/download`) | through the balancer: `/api/v1/gguf/backends/{id}/proxy/...` (since 0.7.34) | on the selected llama.cpp worker |

Before 0.7.34 the text download used a "direct" mode: the URL was built in the
browser as `http://<host>:<port>` and the request flew from the operator's machine
straight into the worker. Hence two failures: for a container name the browser
could not resolve it at all, and with the wrong selection a 13.6 GB model could
land on the wrong machine. Now both paths are identical — through the balancer to
the selected backend.

What is important to remember in any case:

* **the model volumes of different machines are their own** — there is no
  cross-host copying, each model must be downloaded on each host where it is to be
  loaded;
* the HF token for private repositories is set in the WebUI and travels to the
  worker in the `X-HF-Token` header;
* if two backends announce the same `host`, the WebUI shows a warning (a line under
  the URL on the GGUF page and a `⚠` in the selector on the Image page) — this is
  exactly the case when "the models are on disk", although the worker does not have
  them.



### Running on a remote machine

```bash
cd deployments
docker compose -f docker-compose.stack.yml --profile worker up -d cppworker-gpu imageworker
```

The `worker` profile brings up **only** `cppworker-gpu` and `imageworker`. The
external agent is no longer part of it: both workers have their own embedded
metrics collector (`AGENT_EMBEDDED=on`), while the external one is needed only for
**Ollama** backends and is started separately:

```bash
docker compose -f docker-compose.stack.yml --profile legacy-agent up -d agent
```

⚠️ If the external agent is still running on the worker host (for example, it was
brought up by an older version of the compose before it was moved into the
`legacy-agent` profile), **stop it**:

```bash
docker compose -f docker-compose.stack.yml stop agent
```

Two collectors on one record are unacceptable in any case (the metric values start
to "blink"), and in multi-host it is also a second contender for the same ID: the
embedded cppworker agent and the external agent read `AGENT_ID` from the same place
and will fight each other for the record.

### The metrics are collected by the worker's embedded agent

Both workers bring up the collector **in their own process** and register under the
SAME ID as the worker itself — no separate container is needed:

| Worker | Variable | Default | Port |
|---|---|---|---|
| `cppworker-gpu` | `CPPWORKER_AGENT_EMBEDDED` | `on` | `CPPWORKER_AGENT_PORT=18034` |
| `imageworker` | `IMAGE_WORKER_AGENT_EMBEDDED` | `on` | `IMAGE_WORKER_AGENT_PORT=18033` |

⚠️ **Before 0.7.32 this was `off` for imageworker.** On a remote machine that
followed `.env.example` this produced a backend record with **no telemetry at all**:
`hasAgent=false`, `agentPort=18032` (the port of SOMEBODY ELSE'S container) and
`gpuMemory`/`vramUsagePercent` at zero. In the WebUI it looks like this:

```
IMAGEWORKER-34    GPU 0.0%   VRAM 0.0%   POWER 0W   0 MB / 1 MB
```

Checking on the worker — the log must show the embedded agent starting:

```bash
docker logs <imageworker container> 2>&1 | grep -iE 'embedded agent|Agent registered'
# expected:
#   [..] Agent registered successfully: imageworker-34
#   {"msg":"embedded agent started","agentId":"imageworker-34",...,"metricsPort":18033}
```

If these lines are missing, set `IMAGE_WORKER_AGENT_EMBEDDED=on` in
`deployments/.env` on that machine and recreate the worker:

```bash
docker compose -f docker-compose.stack.yml --profile worker up -d --force-recreate imageworker
```

Turning off the embedded agent makes sense only when the metrics of this backend
are collected by the external `agent` container — and never both at once.


### ⚠️ Why the second machine "does not register": identical IDs and host (R-MultiHost, 2026-10-07)

The symptom on the live pair: the second machine (`192.0.2.11`) is up, both workers
are healthy, but the balancer still has **two** records, and the model page shows
the CPU/GPU of the **second** machine under the URL of the **first**. The second
machine serves no requests at all — it looks like "it did not register".

The cause is neither the network nor the token. The compose file is identical on
both machines, so literally every field of the registration request matches:

| Field | On machine 1 | On machine 2 (default) |
|---|---|---|
| `host` (from `AGENT_CPPWORKER_HOST`) | `cppworker-gpu` | `cppworker-gpu` |
| `backendId` / `agentId` | `cppworker-gpu-bundled-agent` | `cppworker-gpu-bundled-agent` |
| `cppWorkerPort` | `18092` | `18092` |
| `SDWORKER_BACKEND_ID` / host | `imageworker` | `imageworker` |

Before the R-MultiHost revision the registration protocol carried **not a single**
feature distinguishing nodes, so the second machine passed the
`isReRegistrationOfAutoBackend` check (host and type match) and **silently rewrote
somebody else's record**: `AgentID`, `AgentPort` and all the metrics. The requests
kept going to the first machine's address, because its URL in the record did not
change — hence the "foreign GPU" on the model page.

Now the balancer distinguishes nodes by the address the registration came from
(`types.Backend.NodeAddr`, see `internal/api/registration_guard.go`) and answers
**409** with instructions instead of silently taking over:

```
Backend "cppworker-gpu-bundled-agent" is already served by node 172.18.0.5
(its agent is alive). Two hosts are using identical backend IDs and advertised
host names because the compose file is the same. On this node (192.0.2.11)
set a unique AGENT_ID (or SDWORKER_BACKEND_ID for the image worker) and
BACKEND_HOST=192.0.2.11, then restart the worker.
```

The guard's rules (so as not to break the regular restart):

* the source matches the record's `NodeAddr` → registration proceeds as before;
* the owning agent has been silent for more than 60 s → the record is considered
  orphaned and takeover is allowed (a recreated container gets a new bridge IP and
  is obliged to re-register);
* records without a `NodeAddr` (created before this revision or manually from the
  WebUI) are not protected — the behavior is as before; the node will be filled in
  on the very first registration.

Banning a single registration would not have solved the problem: the agent
registers **once** at startup, while it sends metrics and heartbeat in a loop.
That is why telemetry is also checked by node — `POST /api/v1/agents/metrics`,
`/agents/heartbeat`, `/agents/v2/*` and `/backends/{id}/agent/*` receive `403`
from a foreign address. This also cures the "blinking" of metrics when two
collectors end up on one backend.

### If a backend disappears from the stand: the worker comes back by itself

Registration used to be one-shot: if the backend record was deleted from the WebUI
or taken over by another node with the same ID, the worker disappeared from the
stand **forever** — until a manual `docker restart`. In the worker log it looked
like this:

```
Metrics send failed with status 404: {"error":"Backend not found. Please register agent first."}
```

Now (since 0.7.31) on `404`/`403` the agent repeats registration no more often than
once every 20 s, so a deleted record comes back by itself within seconds. If the
worker was built by an old build, the only cure is `docker restart <container>`.

### ⚠️ The balancer volume must belong to uid 1000

Editing `state.json` from a helper container **as root** (for example, to set
`nodeAddr` manually) leaves the file root-owned. The balancer runs as uid 1000 and
stops saving state:

```
failed to write state file: open data/state.json: permission denied
```

Checking and fixing:

```bash
docker run --rm -v ol-balancer-data:/data alpine ls -la /data
docker run --rm -v ol-balancer-data:/data alpine chown -R 1000:1000 /data
```


The cure is the same as before: unique `AGENT_ID` and `SDWORKER_BACKEND_ID` plus
`BACKEND_HOST` on the new machine (see above), and delete the extra record in the
WebUI if it is left over from previous attempts.

### Diagnostics in 4 steps

```bash
# 1. From the remote machine: is the balancer reachable (ADMIN API)
curl -sS -o /dev/null -w '%{http_code}\n' http://192.0.2.10:18081/api/v1/ping
#    200 — the network is fine; 000 — firewall or wrong address; 401 — wrong token
#    ⚠️ On the balancer machine inbound TCP 18081 must be ALLOWED

# 2. On the remote machine: what the worker sees
docker logs <container> 2>&1 | grep -E 'registered with balancer|registration conflict|Agent registered'

# 3. On the balancer: has the record appeared
curl -s -H "X-API-Token: $TOKEN" http://localhost:18081/api/v1/backends \
  | jq '.backends[] | {id, host, type, status}'

# 4. From the balancer: does it reach the worker back
curl -s http://192.0.2.11:18092/health
curl -s http://192.0.2.11:18093/health
```

If step 3 still shows the first machine's records, while the metrics in them are
from the second one (compare the CPU model and `gpu.uuids`), the matter is exactly
the matching IDs/host from the table above, not the network: since the second
machine's metrics do arrive, there is connectivity.

If step 1 gives `000` while the workers' `/health` opens from the balancer, the
problem is **exclusively** the inbound port `18081` on the balancer machine.


### ⚠️ Each machine has its own models

The volumes are not shared: the second machine's worker sees **its own**
`${MODELS_DIR}`. A model downloaded on the first machine is absent on the second —
you need either a copy of the files or a network volume. The balancer distributes
REQUESTS, not files.

---

## 5. The variable-name trap (the main reason "the agent does not connect")

### 5.0 Interpolation trap: a compose line does NOT see a neighboring line of the same service

This deserved a separate analysis: `BACKEND_HOST` was set on the remote machine,
everything worked for cppworker, but the image worker still registered under the
container name. The cause was a single compose line:

```yaml
- SDWORKER_ADVERTISE_HOST=${SDWORKER_ADVERTISE_HOST:-${BACKEND_HOST:-imageworker}}   # 1
- AGENT_PUBLIC_HOST=${SDWORKER_ADVERTISE_HOST:-imageworker}                          # 2  TRAP
```

**Compose interpolation is static text substitution** over the host environment and
the `.env` file. Line 2 does not see the value that line 1 assigns to the
CONTAINER: `${SDWORKER_ADVERTISE_HOST}` is read from `.env`, where the variable
does not exist — so the expression always yields the literal `imageworker`. As a
result:

* the worker's self-registration wrote the correct address (line 1 nests
  `BACKEND_HOST`) — the record was created as `192.0.2.11`;
* the embedded agent registered under the same backend ID with `AGENT_PUBLIC_HOST`
  = `imageworker`, and the "backend exists" branch **overwrote the address with the
  container name**.

From there the container name resolves inside the balancer's docker network to ITS
container, and requests to the remote machine silently go to the first one: metrics
arrive from the second machine, the backend looks healthy, and "the models on disk"
are the neighbor's.

**The rule.** Inside `environment:` a variable can interpolate only what the
operator sets in `.env`, or what is nested in the SAME expression:

```yaml
- AGENT_PUBLIC_HOST=${SDWORKER_ADVERTISE_HOST:-${BACKEND_HOST:-imageworker}}   # correct
- FOO=${FOO:-${BAR:-default}}                                                  # correct (self-reference)
- FOO=${BAR:-default}                                                          # correct if BAR is set by the operator
```

Code check: `TestComposeInterpolation_NoSelfServiceReferences` in
`internal/api/compose_interpolation_lint_test.go` walks all compose files and fails
with the file, line and service named. Run:

```bash
go test -tags llama_stub ./internal/api/ -run TestComposeInterpolation
```

A quick manual check that exactly the right values reached the worker:

```bash
# on the balancer machine, with the same BACKEND_HOST as in the remote machine's .env
BACKEND_HOST=192.0.2.11 docker compose -f docker-compose.stack.yml \
  --profile worker config --format json | jq '.services.imageworker.environment
  | {SDWORKER_ADVERTISE_HOST, AGENT_PUBLIC_HOST}'
```

The code reads **not the** names that historically sit in the old compose files. If
you copy a block from an old file, the service will start, but the parameter will
have no effect.

| Set in old files | Read by the code | Consequence of the mistake |
|---|---|---|
| `AGENT_BACKEND_TYPE` | **`BACKEND_TYPE`** | the backend registers as **Ollama**: the balancer takes the Ollama path, and llama.cpp capabilities (KV types, offload, n_ctx ceilings) are not used |
| `AGENT_CPPWORKER_URL` | **`CPPWORKER_URL`** | the agent does not know the cppworker address for metrics |
| `AGENT_NODE_LABELS` | **`NODE_LABELS`** | the labels do not arrive (the UI shows the host's auto labels) |
| `AGENT_MODE` | **`GPU_MODE`** | the GPU mode is determined automatically |
| `AGENT_COLLECT_INTERVAL` / `AGENT_HEARTBEAT_INTERVAL` | **`COLLECT_INTERVAL`** / **`HEARTBEAT_INTERVAL`** | the intervals are taken from the agent defaults |
| `AGENT_BACKEND_ID`, `AGENT_REGISTER_GPU_MODE` | — | not read at all |
| `CPPWORKER_ADVERTISED_PORT` | — | the port for the balancer is set through `AGENT_CPPWORKER_PORT` |
| `BALANCER_PORT`, `BALANCER_API_PORT` | — | the balancer ports come from `config/config.json` |

`docker-compose.stack.yml` uses the correct names.

---

## 5.1 The token: one place from which it spreads to all services

**The source of truth is exactly one: `deployments/.env` → `CPPWORKER_API_TOKEN`.**
The compose file derives everything else from it:

| Service | Variable in the container | Where from |
|---|---|---|
| loadbalancer | `LB_API_TOKEN` | `${CPPWORKER_API_TOKEN}` |
| cppworker | `API_TOKEN`, `CPPWORKER_API_TOKEN` | `${CPPWORKER_API_TOKEN}` |
| agent | `BALANCER_TOKEN` | `${CPPWORKER_API_TOKEN}` |
| agent | `CPPWORKER_API_TOKEN` | `${CPPWORKER_API_TOKEN}` |
| webui | `API_TOKEN` | `${CPPWORKER_API_TOKEN}` |

Change the token — change **only** this line and recreate the stack:

```bash
# deployments/.env
CPPWORKER_API_TOKEN=<new-token>

docker compose -f docker-compose.stack.yml --profile full up -d --force-recreate
```

### What you do NOT need to change (and why it is a trap)

`auth.tokens` in `config/config.json` contains the placeholder `bundled-default`,
and when `LB_API_TOKEN` is set, the token list from the file is **fully replaced**.
Editing `config.json` will have no effect — you will be looking for the cause of a
401 where there is none. The balancer now says so directly in its startup banner
and with a warning in the log:

```
║ Auth tokens:   LB_API_TOKEN (len=33) ЗАМЕНЯЕТ config.json    ║
...
WARN auth.tokens из config.json НЕ ДЕЙСТВУЕТ: список полностью переопределён
     переменной окружения. Меняйте токен в deployments/.env ..., правка
     config/config.json результата не даст.  winner=LB_API_TOKEN config_token_count=1
```

The same is visible for the other services in their banners:

```
cppworker: "apiTokenSource":"API_TOKEN(len=33)"        # or CPPWORKER_API_TOKEN
agent:     ║API token:     CPPWORKER_API_TOKEN/API_TOKEN (len=33)        ║
```

### The cppworker token arrives through the agent

Cppworker protects its endpoints (`/api/models/reload`,
`/api/v1/cppworker/config/update`) and expects `Authorization: Bearer <token>` or
`X-API-Token`. The balancer proxies these requests from the WebUI, but **does not
pass the client token**: it substitutes the backend's server token, and if there is
none it removes the header entirely.

Before the fix, in the bundled stack this token never reached the balancer at all:
the Go-side cppworker registration is disabled there
(`CPPWORKER_REGISTER_DISABLE=true`), the backend is created by the agent, and only
the cppworker registration was able to hand out the token. The result: editing
model parameters in the WebUI through the agent failed with 401 "invalid or missing
API token", even though the token is set in all the `.env` files and is identical.
Now the agent passes `cppWorkerApiToken` in the registration, and the balancer
accepts it on all three paths (create, re-register, attach).

Checking that the path is alive:

```bash
TOKEN=$(grep '^CPPWORKER_API_TOKEN=' .env | cut -d= -f2)
curl -s -o /dev/null -w '%{http_code}\n' -X PUT \
  -H "X-API-Token: $TOKEN" -H 'Content-Type: application/json' -d '{}' \
  "http://127.0.0.1:18080/api/v1/gguf/backends/cppworker-gpu-bundled-agent/proxy/api/v1/cppworker/config/update"
# → 200 (not 401 "invalid or missing API token")
```

---

## 5.2 The version in the log banner: checking "is it really the new build?"

All three services print the version of the running image in the first line of the
banner:

```
╔═════════════════════════════════════════════════════════════╗
║               Ollama Load Balancer - Starting               ║
╠═════════════════════════════════════════════════════════════╣
║Version:       r83-submodule-v23 (commit 5731414, 2026-09-28)║
```

The value comes from `APP_VERSION` (compose substitutes the image tag) plus
`GIT_COMMIT`/`BUILD_DATE`, baked in at build time. From there it is immediately
visible whether the fix was applied:

```bash
docker logs ol-stack-balancer 2>&1 | head -10 | grep Version
docker logs ol-stack-agent    2>&1 | head -12 | grep Version
docker logs ol-stack-cppworker-gpu 2>&1 | grep -m1 'Worker starting'
#   → "version":"r83-submodule-v22 (commit …, …)","apiTokenSource":"API_TOKEN(len=33)"
```

If the version in the log is not the one you built, the container holds an old
image, and looking for a defect in the code is pointless.

---

## 5.3 Your own image registry: one variable, `IMAGE_REGISTRY`

By default the images are taken from the standard path
(`ollama-legion/balancer:<tag>`, built locally). If you have your own registry, its
address is set with **one** variable in `deployments/.env`:

```bash
# deployments/.env
IMAGE_REGISTRY=                       # standard path (default)
IMAGE_REGISTRY=local-docker-hub:5000/ # your own registry — SLASH AT THE END
```

Then compose substitutes the prefix into all four images:

```
local-docker-hub:5000/ollama-legion/balancer:r83-submodule-v23
local-docker-hub:5000/ollama-legion/cppworker:gpu-r83-submodule-v23
local-docker-hub:5000/ollama-legion/agent:r83-submodule-v23
local-docker-hub:5000/ollama-legion/webui:r83-submodule-v23
```

To check what you will get **without starting** anything:

```bash
cd deployments
docker compose -f docker-compose.stack.yml --profile full config | grep 'image:'
```

> ⚠️ **The trailing slash is mandatory.** Compose does not normalize paths: it
> concatenates the strings literally, and without the slash you get
> `local-docker-hub:5000ollama-legion/balancer` — an invalid reference.

**The build reads the same variable.** The scripts add the slash themselves and turn
the placeholders (`local`, `docker.io`, `index.docker.io`) into an empty prefix, so
the built image gets exactly the name compose is looking for:

```powershell
# all images with the release tag, into your registry (address — from deployments/.env)
powershell -File scripts/build-containers.ps1 -Tag r83-submodule-v23

# a one-off build into another registry without editing .env
$env:IMAGE_REGISTRY='other-reg:5000'; powershell -File scripts/build-containers.ps1 -Balancer -Tag test
```

The full release (`scripts/release-all.ps1`) and the point rebuilds
(`rebuild_balancer_r66a.ps1`, `rebuild_cppworker_r66a.ps1`, `rebuild_webui_r66d.ps1`)
also take the prefix into account — otherwise `up -d` would go to the registry for
an image that was just built locally under a different name.

Checking the prefix normalization (without Docker):

```powershell
powershell -File scripts/lib-image-registry.tests.ps1
```

---

## 6. The ordering trap: `environment:` wins over `env_file`

Compose first substitutes `env_file` and then **layers** `environment` on top.
Therefore the line `- LB_FOO=${LB_FOO:-}` in `environment` **wipes** the value from
`.env.bundled-with-agent` with an empty string: the `${...}` interpolation reads
only `deployments/.env`, where such variables do not exist.

The rule: a behavior switch is set **either** in `.env.bundled-with-agent` (and not
mentioned in `environment`), **or** in `environment` with a default from `.env`. In
`docker-compose.stack.yml` the switches (`LB_OPENAI_AUTO_STREAM`,
`LB_AUTO_LOAD_ASYNC`, `LB_AUTO_CONTINUE_*`, `LB_GPU_HEADROOM_PERCENT`) are
deliberately not duplicated.

---

## 7. Capacity: how many parallel requests a backend will accept

The source of truth is cppworker itself:

1. cppworker knows its own parallelism (`CPPWORKER_N_PARALLEL`);
2. the agent, if `AGENT_MAX_CONCURRENT_REQUESTS` is not set (in the compose it now
   defaults to `0` = AUTO), reads it from cppworker
   (`GET /api/v1/cppworker/config` → `config.defaultNParallel`) and reports it to
   the balancer;
3. the balancer lets exactly that many simultaneous requests through, and the rest
   wait in the admission queue.

Therefore you have to configure **one place**. `AGENT_MAX_CONCURRENT_REQUESTS` is
needed only as an **operator limit** (it has priority and is not overwritten by the
automation):

| `CPPWORKER_N_PARALLEL` | What the balancer sees | `AGENT_MAX_CONCURRENT_REQUESTS` |
|---|---|---|
| 0 or 1 | 1 | `0` (AUTO — the default value) |
| 2 | 2 | `0` (AUTO) or `2` as an explicit limit |

An inflated value produces false "free slots": the balancer thinks several requests
can be sent in parallel, while cppworker serves one — the client gets refusals and
hangs. A value `<= 0` means "not set": the agent takes the worker's value, and if
even that cannot be determined, the balancer applies the default for the backend
type (`llama_cpp → 1`).

> ℹ️ Before the R83-fix capacity was set in TWO places, and `environment:` in the
> compose ALWAYS set `AGENT_MAX_CONCURRENT_REQUESTS=1` (the default), so the agent
> never took the value from cppworker. With `CPPWORKER_N_PARALLEL=2` this produced
> exactly the complaint "the first client got a response, the second got a 503": the
> worker held two slots, while the balancer queued the second request.

> ℹ️ Before R83 parallelism was computed by **two different rules**: the C bridge
> (llama.cpp `n_seq_max`) took `CPPWORKER_N_PARALLEL` into account, while the Go
> `SlotManager` took only the explicit `parallel` field from the load request. With
> `CPPWORKER_N_PARALLEL=2` this produced a mismatch: cppworker announced a capacity
> of 2 to the balancer but served one request at a time — the second client silently
> waited in `SlotManager.Acquire` and got a 503. Now both places are computed by one
> function (`internal/cppbackend` → `resolveNParallel`), and it also feeds the
> registration; `cmd/cppworker` has a consistency test.

> ⚠️ **Parallelism > 1: the slots SHARE the window, they do not double the memory.**
> Verified on a live stand (RTX 3070 8 GB, `Qwen3-Instruct-2507-q4km`, `c/llama.cpp`
> `gguf-v0.19.0-1369-g1d2869c6e`, `kv_unified=false` — the default of this build):
> * `parallel=1` — one request is served, the others **queue up**.
>   Measurement: two simultaneous requests, ~3.4 s and ~3.8 s, 7.2 s in total.
> * `parallel=2` — both requests run **truly simultaneously** (they finish at the
>   same moment), both 200, the responses are not mixed up.
> * `parallel=3` — checked: both requests 200, no crashes.
>
> **The VRAM price — there is none.** Same `ctx=8192`, `gpuLayers=20`, `q4_0`:
>
> | parallel | free VRAM, MB | `llama_context: n_ctx` | `n_ctx_seq` (per client) |
> |---|---|---|---|
> | 1 | 5197 | 8192 | **8192** |
> | 2 | 5197 | 8192 | **4096** |
> | 3 | 5191 | 8448 | **2816** |
>
> The reason is in `c/llama.cpp/src/llama-context.cpp`: with `kv_unified=false`
> `n_ctx_seq = GGML_PAD(n_ctx / n_seq_max, 256)`, while the total KV cache is
> computed from `n_ctx` (with `parallel=3` llama.cpp itself adjusts `n_ctx` to
> 8448 = 2816 × 3). That is why `contextSize` in the API and in the WebUI is the
> **TOTAL** window: `16384 + parallel=2` means **8192 per client**, and this was
> never said anywhere.
>
> ✅ **How to set the window PER CLIENT (R83, 2026-09-30).** There is a
> `contextPerSeq` field: if it is > 0, the total `n_ctx = PAD256(contextPerSeq) ×
> slots`, and each client gets exactly what it asked for (memfit checks the total
> size in this case). The same can be set manually: total window = window × slots.
> In the WebUI the "window PER CLIENT" field is in the "Default settings" block, and
> next to `n_ctx` a second line is always shown — "n_ctx per client".
>
> ℹ️ A previous version of this section claimed "the second slot cost ~3.7 GB (a
> second copy of the KV cache)". That was **wrong**: that measurement was taken
> after a reload that changed the layer layout (gpuLayers 20 → 36), and the VRAM
> difference came from the weights, not from the KV. A control measurement with
> identical `gpuLayers` is given in the table above.
>
> ✅ **The previously observed `SIGABRT` (2026-09-29) no longer reproduces.** Back
> then: `parallel=3` + three simultaneous requests **through the balancer** to a
> model that was NOT YET loaded → cppworker crash. Now the same scenario passes:
> three parallel loads in a row (`load-with-params`, `parallel=2`) are accepted and
> deduplicated, `restarts=0`, no `assert`/`abort` in the logs; three simultaneous
> client requests to an unloaded model are served. The cause back then was the load
> context: it was tied to the client that initiated it, and that client's
> disconnection killed the shared load (`model load aborted by user ...
> BRIDGE_ERR_ABORTED`) — now the load context is always detached
> (`loadContextForSharedLoad`).
>
> The balancer's capacity is taken from fact: cppworker returns `max_slots` in
> `/api/models` and announces its parallelism at registration, so the balancer lets
> exactly as many simultaneous requests through as there are slots configured for
> the model (the rest wait in the queue — in the log,
> `admission: request queued, waiting for free slot`). The operator limit
> (`PUT /api/v1/backends/{id}/limits` or `AGENT_MAX_CONCURRENT_REQUESTS`) has
> priority and is not overwritten by the automation.

### 7.0 What can "disturb" the settings set in the WebUI

Verified on a live stand (2026-09-29). The settings are applied once — when the
model is loaded — so it is important to know exactly what changes them:

| Action | Does it affect the settings? |
|---|---|
| Loading/reloading a model from the WebUI (`contextSize`, `parallel`, `kvCacheType`, …) | ✅ applies (that is what the setting is) |
| A client request with **no `num_ctx` at all** (the client did not specify a window) | ❌ **no** — if the model is already loaded, it is not reloaded: we work with what is loaded. If the model is not loaded yet, the load uses the values from the settings (model profile → "default settings" → cppworker env) |
| A client request that the current context **is enough** for (`num_ctx` less than the loaded one) | ❌ no — verified: 16384 loaded, request with `num_ctx=2048` → ctx stayed 16384 |
| A client request that needs **more** context (`num_ctx` > the loaded one) | ⚠️ yes — the balancer starts an auto-reload and raises the context. Verified: 16384 → request with 32768 → ctx became 32768 |
| AutoTune (automatic load optimization) | ⚠️ may reload the model according to its recommendations; there is a cooldown of 5 min after success and 60 s after an error |

> ℹ️ The rule "the client says nothing → we do not touch it" was added in R83
> (2026-09-30). Before it, with `body_num_ctx=0` the preflight substituted the
> profile's `contextLength` (which is a HINT) and, on a mismatch with the loaded
> value, started an async reload: the operator loaded a model with ctx=8192, and the
> very first request without `num_ctx` reloaded it to 16384. In the log this is
> visible as `preflightNCtxReload: клиент не задал num_ctx, модель уже загружена —
> reload не нужен` (previously — `detected n_ctx mismatch, scheduling async reload`).

To make it impossible for client requests to **inflate** the context above your
setting, set the auto-reload ceiling in `deployments/.env`:

```bash
LB_NCTX_RELOAD_MAX_N_CTX=16384   # = your contextSize setting
```

Then a request with a larger `num_ctx` gets a clear refusal (413 naming the ceiling)
instead of a silent model reload. To forbid auto-reload entirely:
`LB_NCTX_RELOAD_ENABLED=false`.

To keep AutoTune away from the parameters: in `config/config.json` either
`balancing.autoTune: false` (globally), or `"autoTune": false` on a specific
profile.

> ℹ️ Known hole (fixed in the code, awaiting rollout): the ceiling
> `LB_NCTX_RELOAD_MAX_N_CTX` was checked only by the reload coordinator, while the
> second path (`preflightNCtxReloadIfNeeded`) ignored it — so in the currently
> running build a client request can still raise the context above the limit. The
> fix is in commit `b9ec496`: when `requested num_ctx > LB_NCTX_RELOAD_MAX_N_CTX`
> a 413 is returned, and the model keeps its previous `n_ctx`.

### 7.1 Where exactly the capacity lands (and why it is sometimes "not heard")

The agent returns `maxConcurrentRequests` in the registration body, and the
balancer accepts it through **three different paths** — and before
`r83-submodule-v23` it updated the capacity in only one of them:

| Registration path | When it fires | Old behavior |
|---|---|---|
| creating a new backend | there is no first backend on this `host:18092` yet | ✅ default by type (`llama_cpp → 1`) |
| re-registering the same `agentId` | the agent was restarted | ❌ kept the previous value |
| **attach** to an already registered cppworker | bundled mode (the agent arrives second) | ❌ kept the previous value |

On top of that the value is saved in `state.json` (the `ol-balancer-data` volume)
and restored at startup. That is why on a stand where a build with the hard-coded
constant `10` had run before, the balancer kept returning `10` even when the agent
had already been recreated with `AGENT_MAX_CONCURRENT_REQUESTS=1`: the value was
taken from `state.json`, the heartbeat handed it to the agent, the agent "applied"
it to itself and returned it in the next registration. A self-sustaining loop.

What to do if in `/api/v1/backends` you see a `maxConcurrentRequests` greater than
your `AGENT_MAX_CONCURRENT_REQUESTS`:

```bash
# 1. Make sure the balancer is exactly v23+ (there capacity is applied on all paths)
docker inspect ol-stack-balancer --format '{{.Config.Image}}'

# 2. Look at what is actually in state.json
docker run --rm -v ol-balancer-data:/d alpine cat /d/state.json | grep -A2 maxConcurrent

# 3. Remove the inherited value and restart the pair
docker exec -u root ol-stack-balancer rm -f /app/data/state.json
docker restart ol-stack-balancer && docker restart ol-stack-agent

# 4. Check: maxConcurrentRequests = 1 is expected
curl -s -H "X-API-Token: $TOKEN" http://127.0.0.1:18081/api/v1/backends \
  | jq '.backends[] | {id, maxConcurrentRequests}'
```

In the balancer log the fix is plainly visible:

```
"msg":"AdoptCapacityFromNode: вместимость бэкенда приведена к n_parallel узла",
"oldMaxConcurrentReqs":10,"newMaxConcurrentReqs":1
```

On a clean install (`ol-balancer-data` is empty) steps 2–3 are not needed: the
backend is created with the correct value right away.

### 7.2 How to enable 2 parallel requests (step by step)

Parallelism is set **in one place** — how many slots cppworker brings up; the
balancer learns it by itself. The order:

```bash
cd deployments
# 1. In .env:
#    CPPWORKER_N_PARALLEL=2
# 2. Restart cppworker (the variable is read at startup):
docker compose -f docker-compose.stack.yml --profile full up -d --force-recreate cppworker-gpu
# 3. Make sure cppworker brought up 2 slots (after the model is loaded):
curl -s http://127.0.0.1:18092/api/models | jq '.models[] | {name, context_size, max_slots}'
#    → "max_slots": 2
# 4. Make sure the balancer sees the same capacity:
curl -s -H "X-API-Token: <token>" http://127.0.0.1:18081/api/v1/backends \
  | jq '.backends[] | {id, maxConcurrentReqs}'
#    → "maxConcurrentReqs": 2
```

> ℹ️ An already loaded model keeps its slots: after changing
> `CPPWORKER_N_PARALLEL` it must be reloaded (unload + request, or "Apply (reload)"
> in the WebUI). Otherwise `/api/models` keeps the previous `max_slots`.

Three ways to set parallelism (priority from top to bottom):

| Way | When to use it | Where |
|---|---|---|
| The `parallel` field in the load request | a one-off load with the required number of slots | WebUI (GGUF models page) / `POST /api/models/load-with-params` |
| The `parallel` field in the model profile | if the profile has `ignoreDefaults: false` | `config/config.json` → `llamaCppModelProfiles.<name>.parallel` |
| `CPPWORKER_N_PARALLEL` | **the main way**: an infrastructure setting of the container | `deployments/.env` |

> ⚠️ If a profile has `"ignoreDefaults": true` (on this stand — all profiles do),
> the profile does **not** substitute load parameters, including `parallel`. That is
> deliberate: `ignoreDefaults` means "the container env wins over the profile" (see
> §7.0). In this mode parallelism is set only by `CPPWORKER_N_PARALLEL`.

> ℹ️ Previously `parallel` from the profile reached cppworker **only** through
> `POST /api/v1/cppworker/model-profiles/{name}/apply` and was lost on a regular
> load (auto-load via `/api/chat`, `POST /api/models/load*`) — the operator saved
> 2 slots in the WebUI, but the model came up with one (`max_slots=1`), and the
> second client got a 503. Fixed in `internal/balancer/model_management.go`
> (`applyProfileLoadParams`), which also has tests.

> ℹ️ Saving a profile in the WebUI does **not reload** the model: the "Save" button
> writes the settings, "Apply (reload)" applies them. R83: after saving, the WebUI
> asks whether to apply right away and explains that otherwise the changes take
> effect on the next model load.

### 7.3 Load settings: what is available and how to assign them to a model from the WebUI

R83 (2026-09-30). Two blocks appeared in the llama.cpp settings (the "Settings"
page → llama.cpp → below Per-Model Profiles):

**"Default settings for new models"** — the very config that used to play the role
of "initialization at balancer start" and is now edited from the WebUI. For each
field the value and its **source** are shown:

| Source | What it means |
|---|---|
| "default settings" | the value is set by the operator (config → `defaultModelProfile`) |
| "container env" | the value is taken from the cppworker env (`CPPWORKER_CTX_SIZE`, `CPPWORKER_N_PARALLEL`, …) |
| "not set" | neither here nor there — cppworker applies its internal default |

Priority when loading a model: **explicit request → model profile → default
settings → cppworker env**. The "Edit" button writes the values into the balancer
config (`PUT /api/v1/cppworker/load-defaults`) — no container restart is needed. The
"Do not apply these values (ignoreDefaults)" checkbox returns control to the
container environment.

**"Models in folder"** — all GGUF files, including those not loaded yet: size,
status, whether they have their own profile (and whether it is disabled through
`ignoreDefaults`), which `n_ctx`/`parallel`/`kv` will be applied to them and where
those came from. The "Configure" button opens the regular profile wizard,
pre-filled with the current values: by saving the profile you thereby **assign the
settings to the file** — they apply on the next load or via "Apply (reload)".

```bash
TOKEN=$(grep '^CPPWORKER_API_TOKEN=' deployments/.env | cut -d= -f2)
# what is available for loading (values + sources + container env)
curl -s -H "X-API-Token: $TOKEN" http://127.0.0.1:18081/api/v1/cppworker/load-defaults | jq
# models in the folder with effective settings
curl -s -H "X-API-Token: $TOKEN" http://127.0.0.1:18081/api/v1/cppworker/model-catalog | jq '.files[] | {name, loaded, hasProfile, effective, source}'
# set the default settings (example: 16K, q4_0, 20 layers, 2 slots)
curl -s -X PUT -H "X-API-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"contextLength":16384,"batchSize":512,"numGpuLayers":20,"kvCacheType":"q4_0","parallel":2}' \
  http://127.0.0.1:18081/api/v1/cppworker/load-defaults | jq
```

Verified on a live stand: after the `PUT` (16K/q4_0/20 layers/2 slots) a model
loaded WITHOUT explicit parameters came up exactly like that —
`effective n_ctx=16384`, `slot manager initialized maxSlots:2 parallel:2`, and
`/api/v1/cppworker/model-catalog` showed the source `defaultProfile` for all four
fields.

> ⚠️ The values from "default settings" apply to models that do not have their own
> profile. If such a model used to load "as the container is configured" (for
> example, `gpuLayers=20` from the env), while `defaultModelProfile` holds a
> different value (for example, 30) — the behavior will change: now the config
> wins. Check the "Default settings" block before the first load of a new model.

### 7.4 Why a regular Cline request "did not go through" (R83, 2026-09-30)

The symptom on the local stand (RTX 3070 8 GB, gemma-4, `parallel=2`): a regular
Cline request (system prompt + `tools[]`, prompt ≈16k tokens, `max_tokens=8192`)
answered after **4.5 minutes**, the WebUI showed neither the occupied memory nor the
active model, and in the cppworker logs there was one `double free or corruption`
with `SIGABRT`. Four independent defects were analyzed — all are fixed.

**1. Read-only endpoints waited for the generation.** `inst.mu` is held for the
whole inference, while the active-request counter (`getActiveQueries`) was read
under the same mutex; it is called by `GetMetrics` (→ `/api/info`) and `ListModels`
(→ `/api/models`). Live log: twelve `GET /api/info` with
`duration 2m51s / 2m36s / … / 6s` — all of them finished in the same millisecond as
the end of the generation; `GET /api/models` did not answer at all (25 s timeout).
That is exactly why the WebUI and the balancer metrics "froze" and the agent did not
send a heartbeat. Now the counters are atomic and are read without the mutex.

**2. The adaptive strategy computed the budget without the model's own memory.**
The strategy is queried before reloading a model that is occupying VRAM right now;
that memory is not in `env.FreeVRAM`, so for gemma-4 (4.0 GiB of weights, 20 layers
on the GPU, 4629 MB free) the strategy answered `stage=cpu_only, gpuLayers=0`. Now
`handleAdaptiveStrategy` adds the memory of the model being reloaded back into the
VRAM budget (it is freed at the moment of the reload) and logs
`бюджет reload'а учитывает память самой модели`.

> ⚠️ Two pitfalls found while checking this fix (both fixed). (a)
> `handleAdaptiveStrategy` resolves the external name into the canonical
> `…Q4_K_M.gguf`, while `/api/models` returns the name **without** the extension —
> the string comparison did not match, and reclaim was silently not applied. (b)
> `ModelInfo.SizeBytes` in cppbackend is often `0` (the size comes from gguf
> metadata, which is not always filled in — see R60.4), so `os.Stat(m.Path)` is
> needed. Verified on v38: in the log
> `free_vram_before_mb=7097, model_vram_mb=5743, free_vram_for_reload_mb=8191`,
> and the strategy at `n_ctx=65536` returns `partial_offload, gpuLayers=30/42`
> instead of `cpu_only`.

**3. A reload that could not change anything.** The `preflight` compared the prompt
ESTIMATE (`tokencount`, a conservative upper bound) with the slot window: 44 230
against 32 768 → async reload "to 131072". The strategy returned `nCtx=65536`
(exactly the current window) and `gpuLayers=0`, the model was unloaded and loaded
again over 2–3 minutes — without a single gain. It was measured how far the estimate
diverges from the real gemma-4 tokenizer (`/api/v1/cppworker/debug/last-prompt`,
`prompt_tokens`):

| Text | `tokencount.Estimate` | Real tokens | Divergence |
|---|---|---|---|
| Latin | 622 | 204 | ×3.05 |
| JSON/tools | 459 | 229 | ×2.0 |
| Cyrillic | 1058 | 264 | ×4.0 |

Therefore window growth is now **limited by a reachability check**: before a 503 the
balancer asks the same strategy and skips the reload if the window will not grow
(`preflight: рост окна пропущен — reload не дал бы выигрыша`). In that case the
request is served in the current window, and if the prompt really does not fit,
cppworker answers with an honest 413 with the reason.

> ⚠️ The first version of this check also rejected the reload at `stage=cpu_only`
> ("the strategy lowers the GPU offload"). **A live run disproved that:** in a
> reload request `gpuLayers: 0` is interpreted by cppworker not as CPU-only but as
> "take it from the profile" (`sync_profile.go`), after which the auto layout gives
> `new_gpu_layers: -1` → `gpuLayers=22`, `offloaded 22/43 layers` — the layout
> becomes **better**. Rejecting such a reload would only deprive the client of the
> context it needs (8192 → 65536). The cpu_only check has been removed; an explicit
> downgrade (for example, 12 layers against 19) is still filtered out in `DoReload`
> (`clampStrategyToCurrentLayout`).

Verified on a live stand (two heavy Cline requests in a row; balancer v37,
cppworker v37–v38):

1. request #1 with a window of 8192 (the profile hint) → a legitimate growth to
   65536, `preflight: async reload succeeded, new_n_ctx=65536`;
2. request #2 with a window of 65536 → **HTTP 200 without a reload**, in the log
   `preflight: рост окна пропущен — reload не дал бы выигрыша … стратегия даёт
   суммарное окно 65536 при текущем 65536`;
3. during generation `GET /api/models` and `GET /api/info` answer in
   **0.01–0.03 s** (before the fix — minutes or a timeout);
4. `prompt > 64 KB` + `think: true` (`/api/chat`) → HTTP 200, `RestartCount=0`, no
   `double free` in the log.

**4. `double free` in the C bridge when prompt > 64 KB.** The template buffer grows
through `C.realloc` (`ret == -4` — a regular Cline/OpenWebUI request with `tools[]`),
while `defer C.free(unsafe.Pointer(outBuf))` remembers the pointer at the moment of
`defer`; after the block moved, `free` released already-freed memory → glibc
`double free or corruption (out)` → `SIGABRT` of the whole process (stack
`ApplyChatTemplateWithThinking → handleChat → /api/chat`). `recover()` in
`handlers_chat.go` does not catch such an abort — the backend simply restarted, and
the client got `connection refused`. The release is now done through a closure.

Check after the rollout (the same Cline-like requests):

```bash
TOKEN=$(grep '^CPPWORKER_API_TOKEN=' deployments/.env | cut -d= -f2)
# 1. /api/models answers DURING generation (before it did not answer at all)
curl -s -o /dev/null -w '%{time_total}s\n' -H "X-API-Token: $TOKEN" \
  http://127.0.0.1:18092/api/models
# 2. in the balancer log there must be no "triggering ASYNC reload" during a heavy request
docker logs --since 10m ol-stack-balancer 2>&1 | grep -E 'reload|пропущен'
# 3. in the cppworker log there must be no "double free"
docker logs --since 1h ol-stack-cppworker-gpu 2>&1 | grep -c 'double free'
```

---

## 7.5 Context-window and parameter policy (R83 policy, 2026-10-01)

The rule by which the balancer decides "load / reload / refuse". Previously the
window grew by the prompt ESTIMATE (`tokencount` — an upper bound that overstates by
2–4×: 44 230 against the real 16 210 on a live Cline request), so a regular request
went into a reload for 2–3 minutes. Now the decision is made **only from what the
client stated explicitly**.

| Situation | What the balancer does |
|---|---|
| The client requires reasoning (`think: true`), while the model is loaded with it off | **409** listing the differences: `param_differences`, `loaded`, `requested`, `what_to_do`. Silently substituting behavior is not allowed |
| The client **did not specify** `num_ctx` | **NoOp.** No reloads "by estimate"; if the history does not fit, cppworker answers with its own reason and the balancer passes it on |
| The model is **not loaded**, the client specified `num_ctx` | **NoOp**, cppworker loads the model **with the client's window**: `ensureModelLoadedWithNCtx` recomputes "per client → total" (`num_ctx × slots`, aligned to 256) |
| The loaded per-client window is **≥** the requested one | **NoOp.** More is loaded — no reload is needed and there is nothing to notify about |
| The loaded window is **<** the requested one, and it fits within the ceiling | **Reload** to the client's window (the other parameters match) |
| The requested window **does not fit** (GGUF ceiling / operator cap / cppworker cannot give that much) | **413** with a clear text: how much was asked for, how much is available per client with N slots, what to do |

The order of window sources at load time:
**`num_ctx` from the request → model profile → `CPPWORKER_CTX_SIZE` env**
(the ladder is implemented in `sync_profile.go`; the request is not overridden by the
profile).

The units matter: the client's `num_ctx` is the window **per client**, while the
model's `n_ctx` is the **total** (llama.cpp divides it between slots:
`n_ctx_seq = PAD(n_ctx / n_seq_max, 256)`). The conversion is `loadTotalNCtxForRequest`
(cppworker) and `perClientToTotal` (balancer).

### The working stand configuration (small model + 32768 per client)

```bash
# gemma-4 profile: total window 65536 with parallel=2 → 32768 to each client
TOKEN=$(grep '^CPPWORKER_API_TOKEN=' deployments/.env | cut -d= -f2)
curl -s -X PUT -H "X-API-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"contextLength":65536,"batchSize":512,"numGpuLayers":-1,"flashAttn":true,
       "useMmap":true,"ignoreDefaults":true,"kvCacheType":"q4_0",
       "contextLengthAuto":true,"contextLengthMax":131072}' \
  http://127.0.0.1:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M
```

In `deployments/.env`: `CPPWORKER_CTX_SIZE=65536` (the same logic: the default total
window) and `CPPWORKER_N_PARALLEL=2`.

Check (all branches of the policy):

```bash
TOKEN=$(grep '^CPPWORKER_API_TOKEN=' deployments/.env | cut -d= -f2)
# 1. cold load with the client's window: expect context_per_seq=32768, context_size=65536
curl -s -X POST -H "X-API-Token: $TOKEN" "http://127.0.0.1:18092/api/models/unload?name=gemma-4-E4B-it-Q4_K_M"
curl -s -H 'Content-Type: application/json' -H "X-API-Token: $TOKEN" \
  -d '{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"ok"}],
       "options":{"num_ctx":32768,"num_predict":8}}' http://127.0.0.1:18080/api/chat
curl -s -H "X-API-Token: $TOKEN" http://127.0.0.1:18092/api/v1/cppworker/config/runtime \
  | jq '.loaded_models[] | {context_size, gpu_layers}'
# 2. window above the ceiling → 413 with an explanation (not a silent reload)
curl -s -o - -w '\n%{http_code}\n' -H 'Content-Type: application/json' -H "X-API-Token: $TOKEN" \
  -d '{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"hi"}],
       "options":{"num_ctx":131072}}' http://127.0.0.1:18080/api/chat
# 3. think=true with reasoning=off → 409 with param_differences
curl -s -o - -w '\n%{http_code}\n' -H 'Content-Type: application/json' -H "X-API-Token: $TOKEN" \
  -d '{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"hi"}],
       "think":true,"options":{"num_ctx":16384}}' http://127.0.0.1:18080/api/chat
```

### Rollout — with one method

The stand lives under `deployments/docker-compose.stack.yml` (project `ol-stack-*`).
That is why the release also goes through the same compose:

```bash
pwsh -File scripts/release-all.ps1 -Tag r83-submodule-vNN -Services balancer,cppworker
#   → by default: docker compose -f docker-compose.stack.yml --profile full up -d
```

The previous default (`docker-compose.cppworker-bundled-with-agent.yml`, project
`ol-bundled-*`) is a different project on the same ports: `up -d` failed with
"Bind for 0.0.0.0:18080 failed: port is already allocated", and the stand stayed on
the old images. The `-ComposeFile` / `-ComposeProfile` parameters let you point at
another file, but by default the working one is always taken.

---

## 7.6 Generation timeouts: the "the client decides" doctrine (R83, 2026-10-01)

**The rule.** The balancer does NOT cut a generation by its own counter. The
connection lives until one of three events: the client closed the connection
(`r.Context().Done()`), the backend returned an error/EOF, the backend died.
Everything else is "tight error tracking", not "we waited N seconds and got bored".

**What it was and how it ended.** The timeouts were computed from the model size and
the request history: `firstByte = 300s` for the "medium" class
(`ModelLatencyTracker.computeModelStats`), 600s / 1200s / 1800s for larger ones, plus
the values from `config.json` (`firstByteTimeout: 900`, `streamingIdleTimeout: 600`,
`requestTimeout: 600`). Measurements on a live stand (gemma-4, RTX 3070, partial
offload):

| Prompt | Prefill | Result |
|---|---|---|
| 22 285 tokens | 296.4 s | made it **before** the 300-second cutoff |
| 22 547 tokens (a real Cline request) | ~299–300 s | **cut at 299.877 s**, `tokens_sent: 0` |
| 28 405 tokens (after the timeouts were removed) | 394.4 s | **`done:true`**, a full response |

That is, the request was cut exactly at the boundary: 296 s passed, 300 s did not.

**How it is now.** Priority:

1. `LB_STREAMING_NEVER_TIMEOUT=1` — the master switch (it is set in
   `deployments/.env.bundled-with-agent`);
2. explicit opt-in through ENV: `LB_STREAMING_TIMEOUT_SEC`,
   `LB_FIRST_BYTE_TIMEOUT_SEC`, `LB_STREAMING_IDLE_TIMEOUT_SEC`,
   `LB_REQUEST_TIMEOUT_SEC`;
3. the per-model profile (`llamaCppModelProfiles[m].*TimeoutSec`) — a deliberate
   decision for a specific model;
4. otherwise — **0, no timeout**.

The values from `config.json` and the heuristics by GGUF size are no longer applied
automatically: they remain in `ModelLatencyTracker.Recommended*` as diagnostics (they
are visible in the API/logs, but they do not interrupt work).

```bash
# check that the timeouts are really disabled
docker exec ol-stack-balancer sh -c 'env | grep NEVER_TIMEOUT'
# and that a slow request goes through (example: ~28k prompt tokens ≈ 400 s prefill)
docker logs --since 10m ol-stack-balancer 2>&1 | grep -E 'STREAMING_IDLE_TIMEOUT|proxy failed'
```

> ⚠️ The flip side: if a backend hangs "dead" (neither answers nor drops the
> connection), the request will hang until the client itself goes away. For such
> cases there are the explicit opt-in ENV above and the model profile; `GET
> /api/models` (the `active_queries` field) and the monitor help to observe hangs.

### 7.6.1 The same rule for load, pull, reload and the HTTP servers (R83/v67, 2026-10-02)

**The formulation of the doctrine the whole code was cleaned up against:**

> Only **state-polling** timeouts are allowed — "every N seconds check whether it is
> done" (poll interval, health probe, metrics, heartbeat, an idle connection between
> requests). Such a timer sets the frequency of the look and never cancels work.
>
> **Duration caps on work are forbidden**: any `time.After` /
> `context.WithTimeout` / `http.Client.Timeout` that, on expiry, **cuts off** a model
> load, a generation, a pull, a reload, an unload. The sign of a defect is that the
> server returns an error while the upstream continues working: the state diverges,
> the operator sees "the model does not load / the response is truncated / context
> canceled" and looks for the cause blindly.
>
> Instead of a cap — wait for a **terminal state** (success / explicit error /
> backend unavailability / the client connection closing) and return a specific
> error.

What was removed in v67 (each item is a potential "cut on level ground"):

| Place | Was | Why it broke the work |
|---|---|---|
| `cmd/cppworker/main.go` | `WriteTimeout = 30m`, `ReadTimeout = 30s` | cutting the streaming response and the reception of a large body exactly on the clock |
| `cmd/balancer/main.go` (API server, TLS proxy, TLS API) | `WriteTimeout = 60s` / `RequestTimeout+30` | a load/reload/apply lasting more than a minute returned an error, although the work was going on |
| `internal/balancer/model_management.go` | `Client.Timeout = 10m`, the load client = `ModelLoadTimeout` | a pull/load of 15.7 GB does not fit — "Client.Timeout exceeded" while the progress was alive |
| `internal/balancer/auto_pull.go` | `Client.Timeout = 10m`, `PullTimeout = "5m"` | downloading a large model "failed by timeout" in the eyes of the caller |
| `internal/balancer/llamacpp_backend_helpers.go`, `ollama_router.go` (`proxyHTTP`) | 120 s / 30 s | `/api/pull` is a model download, not a fast endpoint |
| `internal/balancer/autoload_wait.go` | hard ceiling 30 min | cut the wait **even with growing progress** |
| `internal/balancer/preflight_nctx.go` | reload wait 240 s | the client's very first request got a 503 "being reloaded" |
| `internal/balancer/nctx_reload.go` (`DoReload`) | `context.WithTimeout(ctx, 0)` with the cap disabled | an immediately expired context → the reload failed before reaching the backend |
| `cmd/cppworker/lazyload.go` | default 30 minutes | cancellation of the shared load → `BRIDGE_ERR_ABORTED` ("load cancelled") |
| `cmd/cppworker/cli_auto_load.go` | `Client.Timeout = 30s` | the CLI reported an error while cppworker kept loading |

**How to bring a cap back if it is really needed** (protection from a buggy
upstream). Only through an explicit environment variable, and when it is armed a
loud `WARN` with the variable name is printed — so that "why was it cut" has an
answer in the log:

| Variable | What it limits |
|---|---|
| `LB_ALLOW_PROFILE_TIMEOUTS` | timeouts from the per-model profiles (WebUI) |
| `LB_ALLOW_REQUEST_TIMEOUTS` | the adaptive per-request proxy timeout |
| `LB_ALLOW_MODEL_OP_TIMEOUT_SEC` | the HTTP client for model operations (pull/push/show/create) |
| `LB_ALLOW_MODEL_LOAD_TIMEOUT_SEC` | the HTTP model load request |
| `LB_ALLOW_LOAD_WAIT_CAP_SEC` | waiting for model readiness (poll loops) |
| `LB_ALLOW_NCTX_RELOAD_TIMEOUT` | context reload/preflight |
| `LB_ALLOW_GGUF_PROXY_TIMEOUT_SEC` | the WebUI → cppworker proxy (load/unload/reload/hf) |
| `LB_ALLOW_RPC_INFER_TIMEOUT_SEC` | distributed inference through the RPC coordinator |
| `LB_ALLOW_RPC_TP_INFER_TIMEOUT_SEC` | tensor-parallel inference (TP) |
| `LB_AUTO_CONTINUE_TIMEOUT_SEC` | auto-continuation of a truncated response |
| `LB_NCTX_PREFLIGHT_WAIT_SEC` | waiting for the first async reload: `N` sec, `0` = do not wait (by default we wait for the terminal state) |
| `LB_AUTO_LOAD_WAIT_SEC` | the auto-load wait budget: `N` sec, `0` = do not wait, a negative value = up to the ceiling |
| `CPPWORKER_WRITE_TIMEOUT` / `CPPWORKER_READ_TIMEOUT` | the HTTP Write/ReadTimeout of the cppworker server (`0` = unlimited, the default) |
| `CPPWORKER_LAZY_LOAD_TIMEOUT_SEC` | the context of the shared (lazy) model load |

The single point of truth in the code is `internal/balancer/timeout_policy.go` (the
registry is there too, along with the `optInTimeoutSeconds` helper that prints a WARN
when a cap is armed).

```bash
# 1. servers: all four values must be 0
docker logs --since 5m ol-stack-balancer 2>&1 | grep -E 'HTTP server timeouts'
docker logs --since 5m ol-stack-cppworker-gpu 2>&1 | grep -E 'HTTP server timeouts'
#   expect: read_timeout_sec=0 write_timeout_sec=0

# 2. the containers must not have armed opt-in caps
docker exec ol-stack-balancer sh -c 'env | grep -E "LB_ALLOW_|LB_NCTX_PREFLIGHT_WAIT_SEC|LB_AUTO_LOAD_WAIT_SEC"'
docker exec ol-stack-cppworker-gpu sh -c 'env | grep -E "CPPWORKER_(WRITE|READ|LAZY_LOAD)_TIMEOUT"'

# 3. live check: a pull/load of a large model goes all the way through
docker logs -f ol-stack-balancer 2>&1 | grep -E 'auto-pull|ensureModelLoadedOnBackend'
```

---

## 7.7 "Model returned empty response": a breakdown from the logs (R83, 2026-10-01)

The symptom: Cline on gemma-4-E4B-it-Q4_K_M answers `Model returned empty response`
(the first breakdown is in §7.4). On the live stand it was analyzed TO THE END: the
Cline session journal (`~/.cline/data/sessions/<id>/*.messages.json`) holds two
DIFFERENT failures.

**Failure 1 — empty model output.** The error text Cline showed is verbatim our
chunk: `model produced an empty response (inference succeeded but output is empty)`.
In the cppworker log it corresponds to a WARN with `raw_len=0`: the model finished
its turn with the very first EOG token and generated not a single byte. Reproduced
by the probe (`debug/streamprobe`) on a Cline request with 18 tools — roughly every
second or third run. The cure: one repeated generation pass
(`CPPWORKER_EMPTY_OUTPUT_RETRY`, default true) + a diagnostic WARN with `raw_len`;
confirmation from the log:

```
WARN  writeChatStreamResponseWithTools: пустой вывод модели (raw_len=0) — повторная генерация  prompt_tokens=2513
INFO  writeChatStreamResponseWithTools: повторная генерация завершена  raw_len=311
```

**Failure 2 — the client dropped before the response.** `Model returned empty
response` is already the text of Cline ITSELF. `/api/v1/cppworker/debug/last-stream`
for a 5-minute attempt: `tokens_sent=0, bytes_written=380, reason=ctx_done_on_write,
duration_ms=306047`. 380 bytes over 306 s is exactly 20 keepalives
(`{"keepalive":true}` every 15 s); the client received not a single response token.
The cause was that the tools branch of `/api/chat` buffered the whole response and
returned it as one final chunk.

**Why the response did not make it in time anyway.** The model ran on 35 layers out
of 42 (7 layers on the CPU) — 5.3–9.4 tok/s. The check showed that memfit overstated
the KV cache 4–6×: in gemma-4 the cache is held by 24 layers out of 42
(`attention.shared_kv_layers=18`), of which only 4 are global (layers 5/11/17/23,
the window pattern `111110×7`), while the remaining 20 live in a 512-token window
with a separate head (`key_length_swa=256`). The estimate "all layers × n_ctx" gave
3.17 GB instead of 294 MiB at n_ctx=65536.

Four layers of one defect were fixed: the KV layout (`KVPlan`,
`internal/cppbackend/kv_layers.go`), carrying the layout into the memfit verdict,
looking up the metadata by the client name (`GetModelMetaResolved`) and applying the
profile on cppworker's lazy load. The live result:

| Metric | Before | After |
|---|---|---|
| Layers on GPU | 35/42 (the lazy path 19/42) | **42/42** |
| `tokenLatencyMs` | 125–1035 | **32** |
| Generation | 5.3–9.4 tok/s | **31.6 tok/s** |
| A Cline-like request (18 tools) through the balancer | 31 s | **4.9 s**, `done_reason=tool_calls` |

Additionally: the tools branch now returns prose in deltas, holding a 32-byte window
around the tool call marker (`CPPWORKER_TOOLS_STREAM_CONTENT`, default true), and in
`c/bridge/bridge.c` the repeat of the whole accumulated output when the antiprompt
fires (duplication of the tail up to ~1 KB) has been removed — instead the tail is
held and every token goes to the client exactly once.

Check (the probe shows the arrival time of every chunk and looks for tail
duplication, tool call marker leakage and broken UTF-8):

```bash
go run ./debug/streamprobe http://127.0.0.1:18080/api/chat debug/probe_tools.json
# expect: DONE reason=tool_calls, no content (the call JSON does not leak into content)
go run ./debug/streamprobe http://127.0.0.1:18080/api/chat debug/probe_prose_tools.json
# expect: hundreds of content chunks, "UTF-8 в ответе корректен", "дублирования хвоста не обнаружено"
```

---

## 7.7.1 Reasoning: the contract and the check (R83/v67, 2026-10-02)

**Where the flag lives.** The "Enable reasoning" checkbox on the GGUF Models page is
written into the model profile (`enableReasoning`) and reaches cppworker in the load
body (`POST /api/models/load`), after which it is visible as `reasoning_enabled` in
`GET /api/models` and as `reasoningEnabled` in the balancer's `loadedModels`.

**Priority** (from senior to junior): the client's explicit `think` in the request →
the per-model flag from the profile (WebUI) → the global `config.EnableReasoning`.

**What the client sees.** The reasoning goes out in a separate field
(`reasoning_content` in the OpenAI-compatible response, `reasoning` / `thinking` in
the native Ollama paths), while the visible answer is in `content`. The markers
(`<|channel>thought`, `<channel|>`, `<think>` and others) do not get into the
response: the streaming parser holds an incomplete tag until it becomes clear whether
it is a tag or ordinary text.

There were two live complaints, both closed in v67/v69:

1. "The flag to enable reasoning was active, yet the reasoning was not applied in the
   response" — prompt assembly looked only at the GLOBAL `config.EnableReasoning` and
   did not read the per-model state (what the checkbox sets) at all. The diagnostic
   that catches this: `GET /api/v1/cppworker/debug/last-prompt` — if there is no
   thinking instruction in `prompt_head` while `reasoning_enabled=true`, that is
   exactly this case.
2. "The response is truncated / the response is garbage" — in the stream the first
   characters of the marker (`<|c`, `h`, `a`, `nn`) went into the VISIBLE content,
   while the rest went into reasoning. Check (the SSE deltas must not contain `<|`
   and `channel`):

```bash
# 1. is the mode enabled for the LOADED model
curl -s http://127.0.0.1:18092/api/models | jq '.models[] | {name, reasoning_enabled}'
#   expect: reasoning_enabled = true

# 2. did the policy reach the prompt (a thinking instruction in system)
TOKEN=$(grep '^CPPWORKER_API_TOKEN=' deployments/.env | cut -d= -f2)
curl -s -H "X-API-Token: $TOKEN" \
  "http://127.0.0.1:18092/api/v1/cppworker/debug/last-prompt" | jq '{model, prompt_head}'
#   expect: prompt_head contains the requirement to reason (<reasoning>...</reasoning>)

# 3. separation in the stream: not a single marker fragment in content
curl -sN -H 'Content-Type: application/json' \
  -d '{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"Think step by step: what is 17*23?"}],"stream":true,"max_tokens":300}' \
  http://127.0.0.1:18080/v1/chat/completions \
  | grep '^data: {' | sed 's/^data: //' \
  | jq -r 'select(.choices[0].delta.content) | .choices[0].delta.content' \
  | grep -E '<\||channel' && echo 'УТЕЧКА МАРКЕРА' || echo 'чисто'
```

**Generation bounds.** `max_output_tokens` (which Cline sends) is the upper bound of
"how much response the client will accept", not the requested length: it can only
reduce `num_predict`, not set it. Otherwise "I will accept up to 64000" turns into
"generate 64000" (tens of minutes on a weak card, a cut by the client timeout, an
"empty response").

---

## 8. Diagnostics: what to look at when problems appear

```bash
# 1. Have all the containers come up
docker ps --format '{{.Names}}\t{{.Status}}' | grep ol-stack

# 2. Has the agent registered (the main check of the wiring)
docker logs ol-stack-agent 2>&1 | grep -E 'Register|regist|error|refus'
#   expect: "Registering llama_cpp backend at <host>:18092" + "... registered successfully"

# 3. Does the balancer see the backend and the correct capacity
TOKEN=$(grep '^CPPWORKER_API_TOKEN=' .env | cut -d= -f2)
curl -s -H "X-API-Token: $TOKEN" http://127.0.0.1:18081/api/v1/backends | jq '.backends[] | {id,status,maxConcurrentRequests}'

# 4. Is cppworker reachable directly
curl -s http://127.0.0.1:18092/health
curl -s http://127.0.0.1:18092/api/models | jq '{count, models: [.models[].name]}'

# 5. Why the request did not go through (balancer logs)
docker logs --since 5m ol-stack-balancer 2>&1 | grep -E 'preflight|auto-load|error|503|reject'

# 6. Which backend served the request
curl -s -D - -o /dev/null ... http://127.0.0.1:18080/api/chat | grep -i x-backend-id
```

| Symptom | Cause | What to do |
|---|---|---|
| `X-Backend-Id` is missing, an immediate 503 | the model is not loaded and the load did not start | see items 2 and 5; check `CPPWORKER_MODELS_DIR` and that the GGUF is present |
| 503 "model load started, retry" | a load is in progress (minutes for 27B) | repeat the request; raise `LB_AUTO_LOAD_WAIT_SEC` if needed |
| A second simultaneous client gets 503 "model is not loaded" / "model load aborted by user" | the load was initiated by the first client and its context was tied to it; or the model came up with `max_slots=1` | R83: the load context is detached (`loadContextForSharedLoad`), `parallel` from the profile reaches cppworker; for 2 slots — `CPPWORKER_N_PARALLEL=2`, see §7.2 |
| The client (Cline) "does not work", the request hangs and breaks | the client timeout is shorter than the 27B load time | see item 2.1: load the model in advance, raise the client timeout |
| `400 unknown field "options"` on `/v1/chat/completions` | the client sends an Ollama field into the OpenAI format | `options` is only for `/api/chat`; in the OpenAI format use `max_tokens`/`temperature` |
| `400 invalid JSON: json: unknown field "thinking"` when **continuing an old session** in Cline | the client returned `message.thinking` in the history — reasoning that it itself received from us (an Ollama field, the balancer returns it normally); before 0.7.67 cppworker rejected the field with a strict decoder | update the cppworker image (≥ 0.7.67): `thinking` and `reasoning_content` are accepted and do not go into the prompt. The same class — `400 unknown field "reasoning_content"` on `/v1/chat/completions` |
| Editing model parameters from the WebUI: 401 "invalid or missing API token" | the balancer does not know the cppworker token (or the backend has an empty `cppWorkerApiToken`) | see item 5.1; check the agent banner (`API token`) and the balancer's |
| The token was changed in `config/config.json`, but the 401 remains | the token list is overridden by `LB_API_TOKEN` | see item 5.1: you must change `deployments/.env` |
| The backend is `unhealthy` | the balancer cannot reach `host:18092` | check `BACKEND_HOST`/the network/the firewall (scenario 2) |
| The backend is registered, but `type=ollama` | `AGENT_BACKEND_TYPE` was set instead of `BACKEND_TYPE` | see item 5 |
| `maxConcurrentRequests=10` with `AGENT_MAX_CONCURRENT_REQUESTS=1` | an inherited value in `state.json` + an old balancer image | see item 7.1 |
| Doubt "was the fix applied / which build is in the container" | the image version is unknown | see item 5.2: the version is printed in the log banner |
| Two backends on one endpoint | the Go registration of cppworker and the agent are both enabled | `CPPWORKER_REGISTER_DISABLE=true` |
| `400 invalid JSON: json: unknown field "thinking"` when **continuing an old session** in Cline | the client returned `message.thinking` in the history — reasoning that it itself received from us (an Ollama field, the balancer returns it normally); before 0.7.67 cppworker rejected the field with a strict decoder | update the cppworker image (≥ 0.7.67): `thinking` and `reasoning_content` are accepted and do not go into the prompt. The same class — `400 unknown field "reasoning_content"` on `/v1/chat/completions` |
| "I saved the backend settings, but the model loads with different parameters" | the form values are the **container env** layer, which is weaker than the model profile and the "default settings for new models"; for a model with `ignoreDefaults` the default layer is not applied at all | look in the backend settings tab at the "What NEW models will get" and "Models in folder" blocks (the SOURCE of every value is shown there) and at the "Make these the priority for new models" button; for a specific model — its profile + "Apply (reload)" |
| The "reasoning" checkbox has no effect on a new load | reasoning has two fields: `enableReasoning` (the prompt policy) and `defaultEnableReasoning` (what the load inherits); the form wrote only the first | since 0.7.68 the form writes both, and `config/update` accepts them. For a specific model — the "Reasoning" field in its profile |

**Where the edits of WebUI settings are stored.** The long-lived layers:
`defaultModelProfile` and the model profiles — in the balancer's `config/config.json`
(they survive everything). The `CPPWORKER_*` env variables in `deployments/.env` are
the source of truth for the container and take **priority** over the file
`config/cppworker-defaults.json` (`cppbackend.LoadConfigFromEnv`: env > file). At the
same time `docker-compose.stack.yml` passes some variables **always**, with a
default: `CPPWORKER_ENABLE_REASONING=${CPPWORKER_ENABLE_REASONING:-false}` and the
same for `CPPWORKER_DEFAULT_ENABLE_REASONING`. So editing such a setting in the
backend form works until the first container restart, and after that the value from
`.env` wins again. If you want it long-lived, set the variable in
`deployments/.env` (or edit the model profile / "default settings"). The
`cppworker-defaults.json` file itself is written into the container's writable
layer: it survives `docker restart`, but not a container recreation. If the form
edits must survive an image update as well, mount the directory as a volume:

```yaml
  cppworker-gpu:
    volumes:
      - ./cppworker-config:/app/config   # cppworker-defaults.json will survive up -d --build
```

---

## 9. The other compose files in the folder

**There is one canonical file — `docker-compose.stack.yml`.** Everything below is
either historical or highly specialized; always start with stack.

| File | Purpose | Status |
|---|---|---|
| `docker-compose.yml` | the earliest minimal variant (balancer + volume only) | historical |
| `docker-compose.full.yml` | the full stack with Ollama backends | historical |
| `docker-compose.bundled-full.yml` | bundled + Ollama + webui | historical |
| `docker-compose.cppworker-bundled.yml` | cppworker + balancer + webui (without an agent) | working, but without GPU metrics |
| `docker-compose.cppworker-bundled-with-agent.yml` | the same + an agent | **has variable-name defects**; superseded by stack |
| `docker-compose.cppworker-with-agent.yml`, `…standalone.yml` | variants of the cppworker+agent pair | historical |
| `docker-compose.cppworker-remote.yml` | cppworker+agent to an external balancer | working (see §3), but stack covers this too |
| `docker-compose.cppworker.yml` | three cppworker variants by profiles: `cpu` (default), `gpu`, `stub` | working; `stub` had an empty `networks:` until 0.7.39 and the file did not parse |
| `docker-compose.llama.cpp.yml`, `…llama.cpu.yml` | running the llama.cpp server without our worker | historical (used by `scripts/deploy-llama-remote.*`) |
| `docker-compose.agent.yml` | starting only the agent (for Ollama backends) | working; in stack the same thing is `--profile legacy-agent` |
| `docker-compose.agent.gpu.yml` | an **overlay** on `agent.yml` (host network, GPU limits) | not a standalone file: `-f docker-compose.agent.yml -f docker-compose.agent.gpu.yml` |
| `docker-compose.rpc.yml` | the RPC coordinator (stage P2) | not running, see /api/v1/placement |
| `docker-compose.cocoindex.yml` | the external cocoindex service | not part of the core |
| `docker-compose.test-stub.yml` | a stub stand for tests (used by `playwright.config.js`) | test |
| `docker-compose.fanout-test.yml` | a stand for checking request distribution | test (see `scripts/test-backend-fanout.ps1`) |

Removed in 0.7.39 as dead (not referenced in a single repository file):
`docker-compose.test-stub-fast.yml` and `docker-compose.test-stub.override.yml`
(the latter also configured `CPPWORKER_REGISTER_NAME`, which the code does not read).
