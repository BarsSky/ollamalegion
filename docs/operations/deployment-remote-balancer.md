# Deploying the llama.cpp backend (cppworker + agent) and verifying request distribution

**Date:** 2026-09-27 · For version `r83-submodule-v22`

The document answers two questions:
1. **where to put the balancer address** so that the backend connects;
2. **how to verify that requests are really distributed** between the backends.

---

## 1. What lives where

| File | Purpose |
|---|---|
| `deployments/docker-compose.cppworker-remote.yml` | cppworker + agent backend, **connection to an external balancer** |
| `deployments/.env.cppworker-remote.example` | Environment template: 3 required values + hardware parameters |
| `deployments/docker-compose.fanout-test.yml` | Stand of **two** backends with a delay — to see the distribution |
| `scripts/test-backend-fanout.ps1` | Sends N parallel requests and shows which backend served each one |

Connection diagram:

```
                 ┌───────────────────────────┐
                 │  BALANCER (external)      │
                 │  18080 client API         │
   client ──────►│  18081 ADMIN API          │◄──── registration + metrics (agent)
                 └─────────────┬─────────────┘
                               │ inference: BACKEND_HOST:18092
                               ▼
                 ┌───────────────────────────┐
                 │  THIS MACHINE             │
                 │  cppworker (llama.cpp)    │
                 │  agent (metrics)          │
                 └───────────────────────────┘
```

## 2. Where to put the balancer address

**One place** — the `BALANCER_URL` environment variable of the **agent** service:

```yaml
# deployments/docker-compose.cppworker-remote.yml → services.agent.environment
- BALANCER_URL=${BALANCER_URL:?задайте BALANCER_URL=http://<хост-балансера>:18081}
- BALANCER_TOKEN=${BALANCER_TOKEN:?задайте BALANCER_TOKEN}
```

Reasons for doing it exactly this way:

- **It is the agent that registers, not cppworker.** cppworker has
  `CPPWORKER_REGISTER_DISABLE=true` set: if both the Go-side registration and the
  agent are enabled, **two** backend records appear for one physical endpoint.
- **Port 18081, not 18080.** 18080 is the client API (OpenWebUI/Cline talk to it),
  18081 is the admin API: backend registration, metrics, heartbeat.
- **The address must be reachable from the agent container.** `http://loadbalancer:18081`
  works only inside the same docker network; a balancer on another machine needs
  its IP/DNS, a balancer on this same host needs `http://host.docker.internal:18081`.

### Second required variable: `BACKEND_HOST`

```yaml
- AGENT_CPPWORKER_HOST=${BACKEND_HOST}   # what the balancer receives as the backend host
- CPPWORKER_ADVERTISE_HOST=${BACKEND_HOST}
```

This is **the address of your machine reachable FROM THE BALANCER**, not the name of
the compose service (`cppworker-gpu` resolves only inside this network). If it is set
incorrectly, the backend registers as «healthy», but every request fails with a timeout.

### ⚠ Trap: agent variable names

Verified against the code (`cmd/agent/main.go`), not against the documentation: different
compose files in the project historically contain names that the code **does not read**.

| In compose it may be | The code reads | Consequence of the mistake |
|---|---|---|
| `AGENT_BACKEND_TYPE` | **`BACKEND_TYPE`** | the backend registers as Ollama: the «Engine: Ollama» banner, the llama.cpp path is not used |
| `AGENT_CPPWORKER_URL` | **`CPPWORKER_URL`** | the agent does not know the cppworker address |
| `AGENT_NODE_LABELS` | **`NODE_LABELS`** | the labels do not arrive (the UI shows auto host labels) |
| `AGENT_MODE` | **`GPU_MODE`** | the GPU mode is detected automatically |
| `AGENT_BACKEND_ID` | — (not read) | the backend id is taken from `AGENT_ID` |

`docker-compose.cppworker-remote.yml` uses the **correct** names; it is worth checking
this when copying blocks from other project files.

## 3. Startup order

```bash
cd deployments
cp .env.cppworker-remote.example .env
# fill in: BALANCER_URL, BALANCER_TOKEN, BACKEND_HOST (+ MODELS_DIR, image tags)
docker compose -f docker-compose.cppworker-remote.yml up -d

# check the registration (on the balancer):
curl -H "X-API-Token: $BALANCER_TOKEN" http://<balancer>:18081/api/v1/backends
#   → a record with host=<BACKEND_HOST>, status=healthy, hasAgent=true
```

Checking that the balancer can actually reach the backend:

```bash
curl -H "X-API-Token: $BALANCER_TOKEN" \
  http://<balancer>:18080/api/chat \
  -H 'Content-Type: application/json' \
  -d '{"model":"<name>","messages":[{"role":"user","content":"hi"}],"stream":false}' -D -
#   → 200 and the header X-Backend-Id: <id of your backend>
```

## 4. How to verify request distribution

### 4.1. Why «all requests to one backend» is normal

The balancer selects a backend **by the maximum score**, not round-robin
(`internal/balancer/backend_selector.go`, `scoring.go`). With almost identical
metrics the same backend wins, and the switch happens only when
its `ActiveReqs ≥ prewarm.triggerLoadThreshold` (by default **0.7** of
`MaxConcurrentReqs`).

Therefore, with fast responses (tens of milliseconds) the active counter does not have
time to grow, and **all** requests go to one backend. This is not a distribution bug —
it is a consequence of the requests not competing. Measured on the stand:

| Response latency | 10 parallel | 20 parallel |
|---|---|---|
| ~50 ms (stub without delay) | one backend: 10/10 | one: 18/20 |
| 2 s (stub with delay) | **7 / 3** | **18 / 2** |

Conclusion: to see the distribution you need **overlapping** requests — long responses
or `n_parallel > 1` with a real load.

### 4.2. Verification stand (two backends)

```bash
# 1. stub build image with delay support
docker build -t ollama-legion/cppworker:stub-delay -f docker/cppworker/Dockerfile.stub .

# 2. stub model into a shared volume (GGUF skeleton script)
docker volume create fanout-models
#   put the file distro-probe.gguf into the volume (see the recipe at the end of the document)

# 3. bring up two backends + the balancer (balancer port 18088)
docker compose -f deployments/docker-compose.fanout-test.yml up -d

# 4. load the model on BOTH backends
for c in fanout-cpp-a fanout-cpp-b; do
  docker exec $c curl -s -H 'X-API-Token: fantoken' -X POST \
    -H 'Content-Type: application/json' \
    -d '{"name":"distro-probe","contextSize":4096}' \
    http://127.0.0.1:18092/api/models/load
done

# 5. check the distribution
pwsh -File scripts/test-backend-fanout.ps1 -Container fanout-bal \
     -Token fantoken -Model distro-probe -Count 10
```

The script shows `X-Backend-Id` for every request, a percentage summary, the number of
responses with `X-Queue-Position` and the load per backend from the admin API.

### 4.3. Checking in production

```powershell
pwsh -File scripts/test-backend-fanout.ps1 `
  -BalancerUrl http://127.0.0.1:18080 `
  -Token <LB_API_TOKEN> -Model <loaded model> -Count 10
```

To make requests overlap on real hardware, ask for a long response
(`"options": {"num_predict": 512}`) or run more parallel requests.

## 5. What to do if there is no distribution

| Symptom | Cause | What to check |
|---|---|---|
| All requests to one backend, responses are fast | The requests do not overlap (see 4.1) | Increase the response length/parallelism — this is expected behaviour |
| All requests to one backend, `active` on it grows to `maxConcurrent` | The 0.7 threshold does not kick in while the slots are free | `maxConcurrentRequests` of the backend (in `/api/v1/backends`) |
| The second backend is always `unhealthy` | The balancer cannot reach it: a wrong `BACKEND_HOST` or the port is closed | `curl` from the balancer to `http://<BACKEND_HOST>:18092/health` |
| Duplicate backends for one endpoint | Both the Go-side registration and the agent are enabled | `CPPWORKER_REGISTER_DISABLE=true` |
| The backend is registered as Ollama | `AGENT_BACKEND_TYPE` was used instead of `BACKEND_TYPE` | See the table in §2 |
| `X-Queue-Position` is empty under contention | There are enough slots | `AGENT_MAX_CONCURRENT_REQUESTS` / `n_parallel` |

## 6. Recipe for a stub model for the stand

For a stand you need a file that cppworker accepts as GGUF. A header and an arbitrary
size are enough:

```python
# python3 mkgguf.py — creates /tmp/m/distro-probe.gguf (100 MB)
import struct, os
p = '/tmp/m/distro-probe.gguf'
f = open(p, 'wb')
def s(k, v):
    f.write(struct.pack('<Q', len(k))); f.write(k.encode())
    f.write(struct.pack('<I', 8)); f.write(struct.pack('<Q', len(v))); f.write(v.encode())
def u32(k, v):
    f.write(struct.pack('<Q', len(k))); f.write(k.encode())
    f.write(struct.pack('<I', 4)); f.write(struct.pack('<I', v))
f.write(b'GGUF'); f.write(struct.pack('<I', 3))
f.write(struct.pack('<Q', 0)); f.write(struct.pack('<Q', 6))
s('general.architecture', 'qwen3')
u32('qwen3.block_count', 32)
u32('qwen3.attention.head_count', 24)
u32('qwen3.attention.head_count_kv', 4)
u32('qwen3.embedding_length', 4096)
u32('qwen3.context_length', 32768)
u32('qwen3.attention.key_length', 128)
f.close()
os.truncate(p, 100 * 1024 * 1024)
```
