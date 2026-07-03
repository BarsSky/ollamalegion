# Cline Skill: End-to-end test of OpenWebUI → balancer → cppworker GPU (llama.cpp)

**When to use**
- User asks to test the full request flow: OpenWebUI → balancer port 18080 → cppworker GPU backend.
- Need to verify model loading, non-empty response bodies, and correct routing for llama.cpp backend.
- Working with `deployments/docker-compose.full.yml --profile gpu` on WSL + Docker Desktop.

---

## Step 1 — Ensure cppworker GPU image exists

```bash
wsl -d Ubuntu -e bash -c "docker images ollama-legion/cppworker:gpu-86 --format '{{.Repository}}:{{.Tag}}'"
```

If missing, build it first (the user usually already has a working build; do not rebuild unless asked).

---

## Step 2 — Start the full GPU stack

```bash
wsl -d Ubuntu -e bash -c "cd /home/knaga/ollamalegion && docker compose -f deployments/docker-compose.full.yml --profile gpu up -d"
```

Wait for containers:

```bash
wsl -d Ubuntu -e bash -c "cd /home/knaga/ollamalegion && docker compose -f deployments/docker-compose.full.yml --profile gpu ps"
```

Expected: `loadbalancer` healthy, `cppworker-gpu` at least running.

---

## Step 3 — Verify balancer health and backend registration

```bash
wsl -d Ubuntu -e bash -c 'echo "=== balancer health ==="; curl -s http://localhost:18081/api/v1/health | python3 -m json.tool; echo; echo "=== backends ==="; curl -s http://localhost:18081/api/v1/backends | python3 -c "import json,sys; [print(b[\"id\"], b[\"status\"], b[\"host\"], b[\"cppWorkerPort\"]) for b in json.load(sys.stdin)[\"backends\"]]"'
```

Expected:
- `status` = `healthy`
- `healthyBackends` >= 1
- cppworker backend `status` = `healthy`, `cppWorkerPort` = `18091`

If cppworker is `unhealthy` with `cppWorkerPort=18092` or `host=0.0.0.0`, fix env/registration:

1. Check `deployments/.env`:
   - `CPPWORKER_PORT=18091`
   - `CPPWORKER_HOST` should NOT be set to `cppworker-gpu`
2. Check `deployments/docker-compose.full.yml` for `cppworker-gpu`:
   - `CPPWORKER_HOST=0.0.0.0`
   - `CPPWORKER_ADVERTISE_HOST=cppworker-gpu`
   - `CPPWORKER_ADVERTISED_HOST=cppworker-gpu`
   - `CPPWORKER_ADVERTISE_PORT=18091`
   - `CPPWORKER_ADVERTISED_PORT=18091`
3. Check `docker/cppworker/register-with-balancer.sh` registers `host` from `CPPWORKER_ADVERTISED_HOST`.

After changes, recreate stack and remove stale backends:

```bash
wsl -d Ubuntu -e bash -c "cd /home/knaga/ollamalegion && docker compose -f deployments/docker-compose.full.yml --profile gpu down -v && docker compose -f deployments/docker-compose.full.yml --profile gpu up -d"
```

---

## Step 4 — Provide a GGUF model

Option A: copy an existing model into the project `models/` directory (requires root):

```bash
wsl -d Ubuntu -e bash -c "sudo cp /path/to/model.gguf /home/knaga/ollamalegion/models/ && sudo chown $(id -u):$(id -g) /home/knaga/ollamalegion/models/model.gguf"
```

Option B: temporary bind-mount another models directory by editing the compose volume:

```yaml
volumes:
  - /path/to/models:/app/models:ro
  - hf-cache-gpu:/app/.cache/huggingface
```

Then recreate `cppworker-gpu`.

---

## Step 5 — Verify model discovery

Direct cppworker:

```bash
wsl -d Ubuntu -e bash -c "curl -s http://localhost:18092/api/models | python3 -m json.tool"
```

Through balancer:

```bash
wsl -d Ubuntu -e bash -c "curl -s http://localhost:18080/api/tags | python3 -m json.tool"
```

WebUI nginx proxy:

```bash
wsl -d Ubuntu -e bash -c "curl -s http://localhost:18030/api/tags | python3 -m json.tool"
```

All three should list the same model.

---

## Step 6 — Load a model

Get a healthy backend ID first:

```bash
BACKEND=$(curl -s http://localhost:18081/api/v1/backends | python3 -c 'import json,sys; print([b["id"] for b in json.load(sys.stdin)["backends"] if b["status"]=="healthy"][0])')
echo $BACKEND
```

Load through the balancer GGUF proxy:

```bash
wsl -d Ubuntu -e bash -c 'BACKEND=$(curl -s http://localhost:18081/api/v1/backends | python3 -c "import json,sys; print([b[\"id\"] for b in json.load(sys.stdin)[\"backends\"] if b[\"status\"]==\"healthy\"][0])"); curl -s -X POST "http://localhost:18081/api/v1/gguf/backends/$BACKEND/proxy/api/models/load" -H "Content-Type: application/json" -d "{\"name\":\"MODEL_NAME.gguf\",\"ctx_size\":4096,\"gpu_layers\":-1}" | python3 -m json.tool'
```

Replace `MODEL_NAME.gguf` with the actual filename.

---

## Step 7 — Test generation and check non-empty bodies

### Ollama `/api/chat` stream through balancer

```bash
wsl -d Ubuntu -e bash -c 'curl -s -N -X POST http://localhost:18080/api/chat -H "Content-Type: application/json" -d "{\"model\":\"MODEL_NAME.gguf\",\"messages\":[{\"role\":\"user\",\"content\":\"Say hello in one word.\"}],\"stream\":true,\"options\":{\"temperature\":0.5}}"'
```

Expect NDJSON stream with non-empty `message.content` tokens.

### OpenAI `/v1/chat/completions` non-stream

```bash
wsl -d Ubuntu -e bash -c 'curl -s -X POST http://localhost:18080/v1/chat/completions -H "Content-Type: application/json" -d "{\"model\":\"MODEL_NAME.gguf\",\"messages\":[{\"role\":\"user\",\"content\":\"Say hello in one word.\"}],\"stream\":false,\"temperature\":0.5}" | python3 -m json.tool'
```

Expect:

```json
{
  "choices": [{
    "message": { "content": "Hello.\n", "role": "assistant" }
  }]
}
```

### Ollama `/api/generate` (legacy)

```bash
wsl -d Ubuntu -e bash -c 'curl -s -X POST http://localhost:18080/api/generate -H "Content-Type: application/json" -d "{\"model\":\"MODEL_NAME.gguf\",\"prompt\":\"Say hello in one word.\",\"stream\":false,\"options\":{\"temperature\":0.5}}\" | python3 -m json.tool'
```

Known limitation: this endpoint currently returns empty `response` in cppworker. Prefer `/api/chat` or `/v1/chat/completions`.

---

## Step 8 — Acceptance criteria

- `GET /api/v1/health` on 18081 returns `healthy` with at least one healthy backend.
- `GET /api/tags` on 18080 lists the loaded GGUF model.
- `POST /v1/chat/completions` on 18080 returns non-empty `choices[0].message.content`.
- `POST /api/chat` stream on 18080 emits non-empty NDJSON chunks.
- WebUI on 18030 proxies `/api/tags` correctly.

If any step fails, inspect:

- `docker compose -f deployments/docker-compose.full.yml --profile gpu logs cppworker-gpu`
- `docker compose -f deployments/docker-compose.full.yml --profile gpu logs loadbalancer`

---

## Quick grep patterns

```bash
# Find registration/advertise logic
grep -R "CPPWORKER_ADVERTISE" docker/ deployments/ internal/cppbackend/ cmd/cppworker/

# Find balancer health check URL construction
grep -R "CppWorkerPort" internal/balancer/ --include="*.go"

# Find Ollama/OpenAI proxy routes
grep -R "HandleFunc.*api/generate\|HandleFunc.*v1/chat\|HandleFunc.*api/chat" internal/api/ --include="*.go"