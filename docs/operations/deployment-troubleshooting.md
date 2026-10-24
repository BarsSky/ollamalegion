# Deployment: incident postmortems and traps

This file is the "long-term memory" of OllamaLegion deployments. It holds the
postmortems of live incidents that used to sit as comments right in
`docker-compose.stack.yml`. The compose file itself is deliberately trimmed and
translated to English: it should be readable in a minute, while the details live here.

Short "what to run" instructions are in [deployment-stack.md](deployment-stack.ru.md).
The detailed multi-machine deployment scenario is there too, §4.4.

---

## 0. Console noise in the WebUI: what is ours and what is not

Before looking for the cause in the project, split the messages by source.

**Ours** — lines whose module name contains a version token, for example
`index.js?v=v0.7.36-multihost:180 [i18n] ...` or
`gguf-renderer-refresh.js?v=v0.7.36-multihost:...`. The `?v=` token is substituted by
the Dockerfile when the webui image is built, so it shows which build is answering.
If the version in the console differs from `WEBUI_TAG` in `deployments/.env`, the
browser is holding old modules and Ctrl+F5 is needed.

**Not ours** — lines without a version token, for example `contentscript.js:14083
MaxListenersExceededWarning` together with `ObjectMultiplex - orphaned data for stream
"app-init-liveness"`. `contentscript.js` is a content script of a browser extension
(wallet, password manager); it has nothing to do with the project's WebUI, and such
warnings are safe to ignore.

Separately about `[i18n] Missing translation key: <key>` — this is OUR message.
`webui/js/i18n/index.js` writes a warning when a translation is missing and
**returns the key itself**, so wherever the call is made without a fallback wrapper,
the screen will show `settings.profiles.step_busy` instead of the text. Translation
completeness is checked by `internal/api/i18n_keys_lint_test.go`:

```bash
go test -tags llama_stub ./internal/api/ -run TestI18nKeys
```

---

## 1. ⚠️ Variable names: what the code actually reads

### 1.0 The machine was turned off, but the backend stayed "healthy"

Symptom: a remote machine is shut down, but its backend in the WebUI remains `healthy`
and shows `52 °C`, `405 MHz`, `1.4% VRAM` — the numbers of the last poll, presented as
current ones. This is fixed from two sides, and both matter.

**Why the record may not have been disabled.** The health check builds the URL from the
record's `Host` field. If `Host` is a container name (`imageworker`), then inside the
balancer's docker network it resolves to ITS OWN container: the check hits the wrong
place and always succeeds, no matter how long the remote machine has been off. This is
a consequence of the interpolation trap analyzed in `docs/operations/deployment-stack.ru.md` §5.0 — check
the `host` of the records:

```bash
curl -s -H "X-API-Token: $TOKEN" http://localhost:18081/api/v1/backends?includeUnhealthy=true \
  | jq '.backends[] | {id, host, status, lastAgentContact}'
```

`host` must be the address of the machine (`192.0.2.11`), not a container name.
Since 0.7.37 the balancer additionally does not trust the HTTP check for such records:
if the same `Host` is declared by two backends of DIFFERENT nodes, the state is
determined by the liveness of the backend's own agent rather than by the URL.

**Why the old metrics hung around.** Before 0.7.37 the agent watchdog cleared only the
`hasAgent` flag, while the last reported GPU/CPU/VRAM stayed in the balancer's memory
forever. Now, when the agent goes silent, the metrics are cleared, and the UI shows a
dash instead of a frozen value.

To check that the record is really orphaned and not just cold:

```bash
docker logs ol-stack-balancer 2>&1 | grep -E 'agent timeout|agent is silent'
```

### 1.1 A text (gguf) backend has no GPU temperature and clocks

Before 0.7.37, llama.cpp backends always had zero GPU utilization, temperature, power
and clocks, while the image backend on THE SAME card showed real values. Cause: for
llama.cpp, GPU metrics are taken from `cppworker` (`/api/gpu`), and that returns only
the memory size; a local nvidia-smi/NVML poll was not performed if cppworker returned
at least one card. Now the sources are merged: memory, temperature, clocks and
utilization come from the local poll inside the worker container, and the engine
snapshot remains the fallback (see §1.2 — why it is done that way).

The fix lives in the agent INSIDE the worker, so an updated cppworker image is needed:
you can verify it by the `Metrics collected` line in its log or by the
`gpu.temperature` field in `/api/v1/backends`.

### 1.2 Agents of two workers on one machine show DIFFERENT VRAM

Symptom: in "Backend resources" `cppworker` and `imageworker` show different values on
the very same card — for example `1.1 GB / 8.0 GB (13.4%)` versus
`1.7 GB / 8.0 GB (20.7%)`.

Cause (before 0.7.38): the two agents took memory from **different sources**.

| Backend | VRAM source | freshness |
|---|---|---|
| `image_cpp` | `nvidia-smi`/NVML inside the worker container | live value |
| `llama_cpp` | `cppworker` → `/api/gpu` → `cudaMemGetInfo` | **snapshot at container start** |

`cppworker` calls `bridge.GetGPUInfo` once at initialization
(`internal/cppbackend/backend.go`) and afterwards serves that snapshot in `/api/gpu`
(`GetGPUDevices`/`GetGPUMetrics` read the `b.gpuDevices` cache). Checked in a row:
nvidia-smi changed `1559 → 1662 → 1565 MB`, while `/api/gpu` returned unchanged
`used=1094, free=7097` the whole time. On the second machine the effect was even more
noticeable: an idle RTX 5060 with a real 140 MB was shown as 1117 MB used.

Why it is done that way: `cudaMemGetInfo` is a serialized CUDA call, and on the hot
path (memfit budget) it blocked `/api/models` for 8+ seconds during active inference
(see `internal/cppbackend/memfit_adapter.go`). That is why the measurement was moved to
startup. But as a **metric for the operator** such a snapshot is not good enough.

How it is fixed (0.7.38): the agent for llama.cpp backends supplements the engine data
with a local `nvidia-smi`/NVML poll — which runs anyway for temperature and clocks —
and takes memory from it as well. The engine snapshot remains the fallback when the
local poll is unavailable. Now both agents on one machine show the same live value.

Check after updating the worker images:

```bash
# both backends of one machine must show the same value that CHANGES over time
for i in 1 2 3; do
  curl -s -H "X-API-Token: $TOKEN" http://localhost:18081/api/v1/backends \
    | jq -r '.backends[] | "\(.id) \(.gpu.memoryUsed)/\(.gpu.memoryTotal) MB"'
  sleep 5
done
```

### 1.3 Variable names the code does not read

The project has seen names that the code **does not read at all**: the variable looks
set but affects nothing. This is the most frequent cause of "the agent won't connect"
and "the backend registered as Ollama".

### agent (`cmd/agent/main.go`)

| Set in old files | The code reads | Consequence of the mistake |
|---|---|---|
| `AGENT_BACKEND_TYPE` | **`BACKEND_TYPE`** | the backend registers as **Ollama**: the balancer goes down the Ollama path, llama.cpp capabilities (KV types, offload, n_ctx ceilings) are not used |
| `AGENT_CPPWORKER_URL` | **`CPPWORKER_URL`** | the agent does not know the cppworker address for metrics |
| `AGENT_NODE_LABELS` | **`NODE_LABELS`** | the labels do not arrive (the UI shows host auto-labels) |
| `AGENT_MODE` | **`GPU_MODE`** | the GPU mode is determined automatically |
| `AGENT_COLLECT_INTERVAL` / `AGENT_HEARTBEAT_INTERVAL` | **`COLLECT_INTERVAL`** / **`HEARTBEAT_INTERVAL`** | the intervals are taken from the agent defaults |
| `AGENT_BACKEND_ID`, `AGENT_REGISTER_GPU_MODE` | — | **not read at all** |

Read unconditionally: `BALANCER_URL`, `BALANCER_TOKEN`, `AGENT_ID`, `AGENT_PORT`,
`AGENT_PUBLIC_HOST`, `AGENT_MAX_MODELS`, `AGENT_MAX_CONCURRENT_REQUESTS`,
`AGENT_WEIGHT`, `NVML_ENABLED`, `AGENT_CPPWORKER_HOST`, `AGENT_CPPWORKER_PORT`.

### cppworker (`cmd/cppworker`, `internal/cppbackend`)

- read: `CPPWORKER_*`, `API_TOKEN`, `CPPWORKER_BALANCER_URL`,
  `CPPWORKER_BALANCER_TOKEN`, `CPPWORKER_ADVERTISE_HOST`;
- `CPPWORKER_REGISTER_NAME` — the **backend record ID**; it also becomes `AGENT_ID`
  of the embedded agent (the agent must register under the same ID, otherwise the
  balancer will create a second record for the same worker);
- ❌ `CPPWORKER_ADVERTISED_PORT` **is not read**: the port at which the balancer
  reaches the backend is set via `AGENT_CPPWORKER_PORT`.

### imageworker / sdworker

- read: `SDWORKER_BALANCER_URL`, `SDWORKER_BALANCER_TOKEN`,
  `SDWORKER_ADVERTISE_HOST`, `SDWORKER_ADVERTISE_PORT`, `SDWORKER_BACKEND_ID`,
  `SDWORKER_REGISTER_DISABLE`, `SDWORKER_PORT`, `SDWORKER_MAX_CONCURRENT`,
  `SDWORKER_IDLE_UNLOAD_MINUTES`, `SDWORKER_IMAGE_MODELS_DIR`;
- the token is taken from `SDWORKER_BALANCER_TOKEN`, and if it is absent — from
  `BALANCER_API_TOKEN`.

### balancer

- read: `LB_*` (see `.env.bundled-with-agent.example`), `CPPWORKER_API_TOKEN`,
  `CPPWORKER_URL`;
- ❌ `BALANCER_PORT` / `BALANCER_API_PORT` **are not read**: the ports are taken from
  `config/config.json` (`loadBalancer.port` / `.apiPort`) or from the `-port` /
  `-api-port` flags.

### Embedded agent (`internal/agent/embedded.go`)

Enabled by `AGENT_EMBEDDED=on` and lives **in the worker's process** (cppworker or
sdworker). Read: `AGENT_EMBEDDED`, `AGENT_ID`, `AGENT_PUBLIC_HOST`,
`AGENT_EMBEDDED_PORT` (or `AGENT_PORT`), `AGENT_WORKER_PORT`, `BALANCER_URL`,
`BALANCER_TOKEN`, `GPU_MODE`, `NVML_ENABLED`, `METRICS_INTERVAL` /
`COLLECT_INTERVAL`, `HEARTBEAT_INTERVAL`, `AGENT_MAX_CONCURRENT_REQUESTS`,
`AGENT_WEIGHT`, `API_TOKEN` / `CPPWORKER_API_TOKEN` / `BALANCER_API_TOKEN`.

Default ID: `SDWORKER_BACKEND_ID` (image worker) or `CPPWORKER_REGISTER_NAME`
(llama.cpp worker), otherwise the hostname.

**Why an embedded agent at all.** The external `agent` container registers as a
SEPARATE record and survives the recreation of the worker — hence the
"re-registration" in monitoring. The embedded one lives exactly as long as the worker
and writes metrics into ITS OWN record (`hasAgent=true`, the real `agentPort`).

---

## 2. ⚠️ Variable order: `environment:` overrides `env_file`

Compose first substitutes `env_file`, then **overlays** `environment`. That is why a
line like

```yaml
environment:
  - LB_FOO=${LB_FOO:-}
```

**wipes** the value from `.env.bundled-with-agent` with an empty string: the `${...}`
interpolation reads only `deployments/.env`, where such variables do not exist.

**Rule:** a switch is set EITHER in `.env.bundled-with-agent` (and then it is not
mentioned in `environment`), OR in `environment` with a default from `.env`. In
`docker-compose.stack.yml` the switches are deliberately not duplicated.

---

## 3. 🔑 Token: exactly one place

`CPPWORKER_API_TOKEN` in `deployments/.env` is the only source. Compose derives from it
`LB_API_TOKEN`, `API_TOKEN` / `CPPWORKER_API_TOKEN` (cppworker and agent) and
`BALANCER_TOKEN`.

**Editing `auth.tokens` in `config/config.json` is useless**: when `LB_API_TOKEN` is
set, the list from the file is fully replaced. The balancer says so in the log header
(`Auth tokens: LB_API_TOKEN … ЗАМЕНЯЕТ config.json`).

**Live incident (R83).** In the bundled stack the backend created an agent, and only it
knew the token that cppworker expects. The token did not reach the balancer → the
balancer removed the authorization header when proxying (the "do not send someone
else's secret" rule) → editing model parameters from the WebUI failed with
`401 invalid or missing API token`, even though the token was set identically in all
`.env` files. Now the token travels in the `cppWorkerApiToken` field of the agent
registration.

---

## 4. 🐳 Your own image repository: `IMAGE_REGISTRY`

All images take their prefix from `IMAGE_REGISTRY` (`deployments/.env`):

```
IMAGE_REGISTRY=                        → ollama-legion/balancer:<tag>
IMAGE_REGISTRY=local-docker-hub:5000/  → local-docker-hub:5000/ollama-legion/balancer:<tag>
```

⚠️ **The trailing slash is mandatory.** Compose concatenates the strings literally, and
without a slash you get `local-docker-hub:5000ollama-legion/balancer` — an invalid
reference. The build scripts (`scripts/lib-image-registry.ps1` → `Get-ImageRegistryPrefix`)
add the slash themselves, and turn placeholder values (`local`, `docker.io`,
`index.docker.io`) into an empty prefix.

---

## 5. ⚠️ `backendEngine: ollama_api` throws out all llama.cpp backends

If the balancer's `config/config.json` has `backendEngine: ollama_api`, then
`getEffectiveBackendType()` returns `ollama`, and `filterBackendsByEffectiveType()`
will throw **all** `llama_cpp` backends out of the output.

**Symptoms:** `GET /api/v1/metrics` → `"backends": []` with `totalBackends=1`
(the WebUI monitor is empty), `GET /api/ps` → `503 no healthy backends for read endpoint`
(this is exactly what OpenWebUI and the ollama CLI use), `/api/v1/cluster/models/loaded`
is empty.

For a stack with cppworker the value must be **`llama_cpp`**. With a genuine mix
(ollama + llama.cpp) it stays `llama_cpp`, while `effectiveBackendType` in
`/api/v1/cluster` becomes empty — that is the sign of "showing all types", not an
error.

---

## 6. ⚠️ The "backends only" scenario requires `BALANCER_URL`

`--profile worker` (backends, balancer on another machine) **will silently fail** if
`BALANCER_URL` is not set: the workers will go to `http://loadbalancer:18081` — a
service name that does not exist in this profile. Registration will repeat forever with
an error in the log.

Set **once** in `deployments/.env`:

```bash
BALANCER_URL=http://192.0.2.110:18081
CPPWORKER_API_TOKEN=<the same token as on the balancer>
```

The value travels to all three places: `CPPWORKER_BALANCER_URL` (cppworker itself),
`SDWORKER_BALANCER_URL` (image worker) and `BALANCER_URL` (embedded agent).

⚠️ `BALANCER_URL` is the **ADMIN API (18081)**, not the client port (18080).

**Live incident.** The `worker` profile did not deploy at all:
`service "imageworker" depends on undefined service "loadbalancer"` — compose
validates `depends_on` even for services outside the active profile. Fixed with
`required: false`: `depends_on` only sets the startup order.

---

## 7. Ports: what to publish externally

| Port | Component | Needed externally? |
|---|---|---|
| 18080 | Balancer, Ollama API | yes (clients) |
| 18079 | Balancer, OpenAI + `/v1/images/*` | yes (clients) |
| 18081 | Balancer, admin API | yes (WebUI, backend registration) |
| 18083 | WebUI | yes (operator) |
| 18092 | CppWorker | **in scenario 2 — yes** (balancer on another machine), in scenario 1 — only inside the network |
| 18093 | ImageWorker | the same as 18092 |
| 18034 | Embedded cppworker agent | inside the network |
| 18033 | Embedded imageworker agent | inside the network |
| 18032 | External agent (Ollama) | inside the network |
| 18094 | sd-server engine | **no**: listens on loopback inside the worker container |

The ports of the embedded agents are separated on purpose: two processes must not
listen on the same port, and the balancer polls metrics via the `agentPort` of a
specific record.

---

## 8. 🔎 VRAM deduplication: several backends on one machine

One host can hold several backends (cppworker + imageworker). Both agents read
`nvidia-smi` of **one** card and send **the same** `memoryTotal`/`memoryUsed`. The
naive sum gave **16 GB on an 8 GB card**.

The sign of "one physical card" is the **UUID** (`gpu.uuids`, from
`nvidia-smi --query-gpu=uuid`): for containers of one host it matches byte for byte.
The hostname does not work for this — the containers register under different names
(`cppworker-gpu`, `imageworker`), that is, for the balancer these are "two machines".

If there is no UUID (non-NVIDIA platform, old agent), deduplication goes by hostname:
one host = one card.

**Important about llama.cpp workers:** their GPU metrics come from cppworker
(`/api/gpu`), where there is no UUID, so the UUID is appended from `nvidia-smi` in
`mapLlamaGPUMetricsWithUUIDs`. Without that, dedup would work only for the image
worker.

---

## 9. 🔎 Auto-unloading of idle models

cppworker keeps **all** loaded models in VRAM: `idleUnloadMinutes=0` means "never
unload". On 8 GB that is a trap.

**Live incident.** Qwen3-Instruct-2507 (5.4 GB) and gemma-4-E4B stayed in memory after
their sessions, free VRAM dropped to 391 MB, and an image generation request was
rejected by the gate: `image model needs 520 MB VRAM, only 391 MB free` (policy
`coexistence=exclusive`). The image model was already **loaded** at that point — it was
130 MB short, occupied by someone else's model.

Fixed by `CPPWORKER_IDLE_UNLOAD_MINUTES=10` in `.env`. After the extra model was
unloaded, free VRAM grew 210 → 1084 MB, and generation succeeded.

⚠️ **Setting the same parameter via the WebUI/API is not persisted**:
`saveConfigToDefaultsFile` writes to `config/cppworker-defaults.json` relative to the
working directory, and there is no `config/` directory in the image. In the log:
`failed to save config to cppworker-defaults.json: no such file or directory`.
The only reliable place is the environment variable.

---

## 10. 🎨 Qwen-Image 2.1: why "an almost complete set" does not work

Qwen-Image 2.1 is a DiT family: one diffusion file is not enough, the engine is loaded
with the flags `--diffusion-model` + `--vae` + `--llm`.

**An error** that looks like "the model is broken" but means exactly one thing — the
`llm` role is not connected:

```
Conditioner model tensor 'text_encoders.llm.model.embed_tokens.weight' not in model metadata
diffusion_engine.cpp: model metadata validation failed
```

Mechanics: the file from `--llm` is connected with a prefix
(`init_from_file(llm_path, "text_encoders.llm.")`), so the suitable file is one whose
tensors are already named `model.embed_tokens.weight*`. MLX variants of the same
repository do not fit by weight type (`U32`).

Checking that the set is complete — before loading: the HuggingFace tab → "Passport"
(`GET /api/hf/plan`) shows `engineMode` and the mandatory roles.

Checking after loading — that the worker's `argv` has all three flags:

```bash
docker logs ol-stack-imageworker 2>&1 | grep -o '\-\-\(diffusion-model\|vae\|llm\) [^ ]*'
```

Details — [image-generation.md](../features/image-generation.md) §16.10.

---

## 11. 🧩 Two metric collectors on one backend — "flashing" values

If a backend has both an embedded agent (`AGENT_EMBEDDED=on`) and an external `agent`
container running at the same time, they send metrics **in turn**, and the values
flash — in particular `gpu.uuids` is sometimes there and sometimes not, and it is
needed for VRAM deduplication (§8).

Therefore: for llama.cpp workers — only the embedded one (`full`), the external
`agent` — for **Ollama** backends (`worker` / `legacy-agent`).

---

## 12. 🔌 Remote machine: checklist

1. `BALANCER_URL=http://<balancer>:18081` (ADMIN API) in `.env` on the worker machine.
2. `CPPWORKER_API_TOKEN` — **the same** as on the balancer.
3. `CPPWORKER_ADVERTISE_HOST` / `SDWORKER_ADVERTISE_HOST` — the address of the worker,
   **visible from the balancer** (not `localhost`).
4. The worker port (18092 / 18093) is open for the balancer machine — in scenario 2 it
   must be external, in scenario 1 being inside the network is enough.
5. Check on the balancer: `curl -s localhost:18081/api/v1/backends`.
6. On the worker, in the log: `registered with balancer` (and `Agent registered` for
   the embedded agent).

⚠️ Different machines with one card are **not** duplicates, but a match of the physical
card is determined by UUID (§8). Identical `host` + port from different machines must
not be merged.

---

## 13. ⚠️ Image version and commit

The balancer, the agent and cppworker print the tag and the commit as the first line of
the header: `║Version: r83-submodule-v71 (commit 82c4e60, 2026-10-07)║`.

**The image version does NOT tell you the commit.** A live trap: the cppworker image was
built from commit `82c4e60`, while the needed fix appeared in `83b12f3` — it simply was
not in the binary (check with `strings /app/cppworker | grep -c query-gpu=uuid` → 0).
Before rolling out, compare the build commit with the fix commit.
