# Rebuild After Fixes — Docker Container Workflow

> **Last updated:** 2026-06-04
> **Applies to:** all OllamaLegion deployments via `docker compose` / `deploy-agent-docker.sh`

## Why this page exists

Several recently merged fixes (Cline multi-modal content normalization, GGUF
duplicate-download handling, llm-cpp proxy 500 → 200 rewrite) live in the
**Go binary** of the balancer / cppworker / agent. Static assets (webui) are
mounted from the host (see `webui/` in the compose files), so they don't need
a rebuild. **But Go code DOES require a rebuild of the container image** to
take effect.

If you run tests against running containers and the fix "doesn't work", it
usually means one of the following:

1. The container was started with the old image (e.g. via `docker compose up -d`
   without `--build`) → the Go binary inside is the old one.
2. The fix is in the Go binary, but the webui static files were not refreshed
   in the host bind-mount.
3. The browser cached the old JS bundle.

This page gives a deterministic recipe for getting a clean state.

## TL;DR

```bash
# 1. Stop everything
docker compose -f deployments/docker-compose.full.yml down

# 2. Rebuild ONLY the images whose code changed
docker compose -f deployments/docker-compose.full.yml build --no-cache balancer cppworker

# 3. Start fresh
docker compose -f deployments/docker-compose.full.yml up -d

# 4. Verify versions (balancer / cppworker log a build-time stamp)
docker logs ollama-balancer 2>&1 | grep -i "version\|build"
docker logs ollama-cppworker 2>&1 | grep -i "version\|build"
```

If you use the agent-only / single-service compose files, the same pattern
applies — just substitute the service name in the `build` line.

## What is built into which image

| Image              | Source paths                                     | What changes after rebuild |
| ------------------ | ------------------------------------------------ | -------------------------- |
| `ollama-balancer`  | `cmd/balancer`, `internal/balancer`, `internal/api`, `internal/...` | All API behaviour, balancing, normalization, proxy logic |
| `ollama-cppworker` | `cmd/cppworker`, `internal/cppbackend`, `c/bridge` | HF download, GGUF model ops, model load/unload |
| `ollama-agent`     | `cmd/agent`, `internal/agent`                   | Agent-side metric scraping, limits |
| `ollama-webui`     | `webui/*` (mounted as bind, not built into image)| Mounted statically — no rebuild needed |
| `ollama-monitor`   | `cmd/monitor`                                    | Real-time display, history, charts |

## Common fixes that need a rebuild

### 1. Cline multi-modal content → 400 "cannot unmarshal array into string"

**Symptom** in Cline console:
```
400 Bad Request: invalid JSON: json: cannot unmarshal array into Go struct field
openAIChatMessage.messages.content of type string
```

**Fix location:** `internal/balancer/openai_normalize.go` + `internal/balancer/llamacpp_router.go`

**Required:**
```bash
docker compose -f deployments/docker-compose.full.yml build --no-cache balancer
docker compose -f deployments/docker-compose.full.yml up -d balancer
```

### 2. GGUF download — duplicate POSTs return 500 "already in progress"

**Symptom** in browser DevTools → Network:
```
POST /api/v1/gguf/backends/.../proxy/api/hf/download → 500
{"error":"download already in progress for HauhauCS/...gguf"}
```

**Three-layer fix; only ONE of them needs the rebuild for behavior change:**

| Layer | File | Requires rebuild? |
| ----- | ---- | ----------------- |
| Client-side `_downloadInFlight` Map | `webui/js/modules/gguf-renderer.js` | **No** (webui is bind-mounted) — just hard-refresh the browser (Ctrl+Shift+R) |
| Balancer proxy 500 → 200 rewrite | `internal/api/gguf_backend_proxy.go` | **Yes** — rebuild `balancer` image |
| CppWorker `StartDownload` returns 200 with progress | `internal/cppbackend/hf_downloader.go` | **Yes** — rebuild `cppworker` image |

So:
```bash
# 1. Browser hard-refresh to pick up webui changes
# 2. Rebuild balancer + cppworker
docker compose -f deployments/docker-compose.full.yml build --no-cache balancer cppworker
docker compose -f deployments/docker-compose.full.yml up -d balancer cppworker
```

After this, duplicate POSTs get rewritten by the balancer proxy to a clean
200 response with `{"status":"already_in_progress","message":"...","progress":{...}}`.
The browser's client-side guard also stops the second POST from being sent
in the first place.

## Step-by-step verification

### 1. Confirm the new image is running

```bash
docker images | grep ollama
# Look for a recent creation time for ollama-balancer / ollama-cppworker
```

### 2. Confirm the balancer is serving the new logic

Hit the health endpoint and look for the build stamp in the response headers:
```bash
curl -i http://localhost:18080/health
# Should respond 200 OK with Content-Type: application/json
```

Then make a test request that exercises the new logic:
```bash
# Cline-style multi-modal content — should NOT 400 anymore
curl -X POST http://localhost:18080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen2.5-coder",
    "stream": false,
    "messages": [
      {"role": "user", "content": [
        {"type": "text", "text": "Hello"}
      ]}
    ]
  }'
```

If the response is 200 with `choices[]` populated, the new normalization is
active. If it's 400 with the old error, the balancer is still the old image.

### 3. Confirm the cppworker is serving the new download logic

From the webui GGUF page, click "Download" twice rapidly. The network panel
should show:
- **Only ONE POST** to `/api/hf/download` (the second is intercepted by the
  client's `_downloadInFlight` Map), OR
- **Two POSTs both returning 200** with `status: "already_in_progress"`.

If the second POST returns 500 with `already in progress`, the cppworker
image is old.

## Quick reset (when in doubt)

If the system gets into an inconsistent state (stale sessions, old behaviour
hanging around, configs not picked up):

```bash
# 1. Stop and remove containers + network
docker compose -f deployments/docker-compose.full.yml down

# 2. Remove old images (forces a complete rebuild)
docker images | grep ollama | awk '{print $3}' | xargs docker rmi -f

# 3. Rebuild everything from scratch
docker compose -f deployments/docker-compose.full.yml build --no-cache

# 4. Start
docker compose -f deployments/docker-compose.full.yml up -d
```

## Related pages

- `docs/cline-troubleshooting.md` — Cline-specific issue diagnostics
- `docs/troubleshooting.md` — General troubleshooting
- `docs/agent-deployment.md` — Agent deployment workflow
- `DEPLOYMENT.md` — Full deployment guide

## When NOT to rebuild

- **WebUI CSS / JS only changes** — just refresh the browser. The webui is
  bind-mounted into the balancer container at `/var/www/webui`, not baked
  into the image. If you see a permission error in the container logs, check
  the bind mount in the compose file.
- **Config file changes** — `config/config.json` is bind-mounted too. Just
  restart the balancer: `docker compose ... restart balancer`.

## CGo build tag note

If you switch between stub and real `llama.cpp` builds, set the build arg in
the compose file:

```yaml
# deployments/docker-compose.cppworker.yml
services:
  cppworker:
    build:
      context: ..
      args:
        # -D llama_stub   # pure-Go stub, no real llama.cpp
        # (no flag)         # real CGo build (requires build image with libstdc++ dev headers)
```

After changing the flag, **always** rebuild cppworker with `--no-cache`.