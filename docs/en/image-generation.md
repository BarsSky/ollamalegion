# Image generation in OllamaLegion (image backend)

> **Version:** 1.0 (2026-10-02)
> **Status:** Phases 1–6 implemented (types, OpenAI port, routing, worker, WebUI, VRAM gate, discovery)
> **Engine:** `stable-diffusion.cpp` (`sd-server`), contract pinned to `master-929-3f8527a`
> **Related:** [`backend-type-isolation.md`](backend-type-isolation.md), [`../plans/2026-09-27-image-generation-backend-plan.md`](../plans/2026-09-27-image-generation-backend-plan.md), [`../research-sdcpp-lowvram-integration.md`](../research-sdcpp-lowvram-integration.md) (Russian)

## 1. What this is

OllamaLegion serves **two different classes of requests** on different ports:

| Port | Purpose | Who talks to it |
|---|---|---|
| **18080** | universal proxy (as before): Ollama `/api/*`, `/v1/*`, translations | OpenWebUI, CLI, Ollama clients |
| **18079** | **OpenAI surface**: `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, `/v1/models`, `/v1/images/*`, `/sdapi/v1/*` | OpenAI-compatible clients and image generators |
| 18081 | management API (`/api/v1/*`) | WebUI, scripts |
| 18093 | image worker (`sdworker` + `sd-server`) — a separate process | the balancer |

How the OpenAI surface differs from 18080:

- streaming is enabled **only** by an explicit `stream` field in the request body (18080 keeps the legacy `stream: true` default);
- Ollama-native paths (`/api/chat`, `/api/tags`, `/api/pull`, …) are **not served** here — the client gets a 404 pointing at 18080;
- image requests (`/v1/images/*`, `/sdapi/v1/*`, model prefix `sd:`) go **only** to backends of type `image_cpp`;
- text requests (`/v1/chat/completions` and friends) go only to `llama_cpp`/Ollama backends.

State (sessions, queue, scoring, metrics) is **shared** between 18079 and 18080: it is the same `*Proxy`, just two surfaces.

## 2. Registering an image backend

```bash
curl -X POST http://localhost:18081/api/v1/backends \
  -H "Content-Type: application/json" \
  -H "X-API-Token: $LB_TOKEN" \
  -d '{"id":"image-1","name":"image worker","host":"192.0.2.30","imagePort":18093,"backendType":"image_cpp"}'
```

- `imagePort` defaults to **18093** (18091/18092 belong to cppworker instances); when omitted the balancer falls back to `cppWorkerPort` and then to 18093.
- Health probing tries `GET /health` (our worker) and then `GET /sdcpp/v1/capabilities` (a bare `sd-server`, which has no `/health`).
- `image_cpp` is accepted in the `standard`, `replication` and `rpc_coordinator` operating modes. It is deliberately **not** accepted in `virtual_router`/`distributed_inference` (those modes are locked to llama.cpp).

## 3. Connecting clients

### 3.1 SillyTavern (source "stable-diffusion.cpp server")

SillyTavern speaks to `sd-server` natively — no adapter needed.

1. Extensions → Image Generation → Source: **stable-diffusion.cpp server**.
2. URL: `http://<balancer>:18079` (through the balancer) or `http://<worker>:18093` (direct).
3. The **Connect** button issues `OPTIONS /v1/images/generations` — the balancer answers `204`.
4. The model list comes from `GET /v1/models` (it contains `sd-cpp-local`).
5. Generation is sent to `POST /sdapi/v1/txt2img`.

Engine limitations worth knowing: switching models via `POST /sdapi/v1/set-model` returns 500 (there is no model-switch endpoint — the model is chosen when the process starts); some samplers from SillyTavern's list have no sd.cpp equivalent and silently fall back to the default.

### 3.2 Open WebUI

Both engines work:

```env
ENABLE_IMAGE_GENERATION=true
# Option A: OpenAI Images
IMAGE_GENERATION_ENGINE=openai
IMAGES_OPENAI_API_BASE_URL=http://<balancer>:18079/v1
IMAGES_OPENAI_API_KEY=any
IMAGE_GENERATION_MODEL=dall-e-2      # any id from /v1/models
IMAGE_SIZE=512x512                   # default is 512x512
IMAGE_STEPS=20                       # NOTE: Open WebUI default is 50 (minutes on CPU)

# Option B: A1111
# IMAGE_GENERATION_ENGINE=automatic1111
# AUTOMATIC1111_BASE_URL=http://<balancer>:18079
```

`IMAGES_OPENAI_API_BASE_URL` **must** include `/v1`.

### 3.3 LibreChat

```env
# A1111 path (Stable Diffusion tool)
SD_WEBUI_URL=http://<balancer>:18079

# OpenAI Images (agent image tools)
IMAGE_GEN_OAI_BASEURL=http://<balancer>:18079/v1
IMAGE_GEN_OAI_API_KEY=any
IMAGE_GEN_OAI_MODEL=dall-e-2
```

### 3.4 AnythingLLM

The `openai` provider is hardwired to api.openai.com, so use **`localai`**:

```env
IMAGE_GEN_PROVIDER=localai
IMAGE_GEN_LOCALAI_BASE_PATH=http://<balancer>:18079/v1
IMAGE_GEN_LOCALAI_API_KEY=any
IMAGE_GEN_MODEL_PREF=sd-cpp-local
IMAGE_GEN_SIZE_PREF=512x512
```

### 3.5 n8n

An OpenAI credential with `url = http://<balancer>:18079/v1`. Both nodes work: the legacy `OpenAI (image → create)` and LangChain `Image → Generate an Image`. The model dropdown filters ids by the `dall-` prefix, which is why the balancer exposes the `dall-e-2`/`dall-e-3` aliases.

### 3.6 OpenAI SDK

```python
from openai import OpenAI
client = OpenAI(base_url="http://<balancer>:18079/v1", api_key="any")
r = client.images.generate(model="dall-e-2", prompt="a cat", size="512x512", n=1)
print(len(r.data[0].b64_json))   # url is None: sd.cpp returns b64_json only
```

### 3.7 What does not work

- **Home Assistant** — supports the official OpenAI Responses API only, no `base_url`.
- **Jan** — has no image generation feature.
- **ComfyUI transport** in LobeChat/Cherry Studio — expects an actual ComfyUI.

## 4. Request handling (normalization)

| Field | How it is handled |
|---|---|
| `model` | any value is accepted and **ignored** by the engine (one model per `sd-server` process) |
| `size` | `"auto"`/`""`/`"WxH"`; clamped to 64…4096 and rounded to a multiple of 64 |
| `n` / `batch_size` | 1…8 (the engine clamps silently); `data[]` always has exactly as many entries as were produced |
| `steps` | 1…100 |
| `seed` | when omitted, resolved to a random positive number. **This normalization is mandatory:** sd.cpp's OpenAI handler does not read `seed` at all and defaults to 42, so without it every image would be identical |
| `response_format` | `b64_json` or absent → base64; `url` → we store the PNG and return a URL (sd.cpp only ever returns base64) |
| `output_format`, `output_compression` | passed through (`png`/`jpeg`/`webp`, 0…100) |
| input images (init/mask) | PNG and JPEG pass through unchanged; **WebP is decoded and re-encoded to PNG** (the engine contract is PNG/JPEG). If a webp file cannot be decoded the response explicitly asks for PNG, and a mask is never forwarded to the engine |
| `quality`, `style`, `user`, `background` | ignored |
| errors | OpenAI envelope `{"error":{"message","type","code"}}` (sd.cpp returns `{"error":"string"}`) |

Cancellation works only for jobs in the `queued` state; a generation already running cannot be interrupted (409) — that is an engine limitation, not a balancer one. The engine reports no per-step progress either.

## 5. VRAM gate and coexistence with text inference

A diffusion model and a text LLM usually do not fit in one GPU's VRAM at the same time (FLUX Q4 peaks at 3.7–6.4 GB, Q8 up to 12 GB). The balancer therefore (1) checks whether the image model fits and (2) serializes the two load classes.

Configuration — the `balancing.image` section:

```json
{
  "balancing": {
    "image": {
      "coexistence": "exclusive",
      "vramHeadroomMb": 512,
      "blockOnUnknownVramEstimate": false,
      "queueWaitTimeoutSec": 30,
      "exclusiveLockTimeoutSec": 600,
      "gateDisabled": false
    }
  }
}
```

| Field | Meaning |
|---|---|
| `coexistence` | `exclusive` (default) — during generation the image backend owns the card, text slots on the same host are withheld; `offload` — generation runs with RAM offload, both work together; `dedicated` — the image backend has its own GPU, no restrictions |
| `vramHeadroomMb` | VRAM to keep free on top of the model estimate |
| `blockOnUnknownVramEstimate` | `false` (default) — when the estimate is unavailable, generation is **allowed** (a WARN is logged); `true` — strict mode, fails with `unknown_vram_estimate` |
| `queueWaitTimeoutSec` | how long `exclusive` waits for the GPU before answering `429 image_gpu_busy` + `Retry-After` |
| `exclusiveLockTimeoutSec` | safety valve: the lock is force-released (with a WARN) so a hung generation cannot block the card forever |
| `gateDisabled` | disable both the gate and the lock (e.g. image generation runs on CPU) |

How the estimate is derived (in priority order): the model profile (`vramEstimateMb` in `image-model-profiles.json`) → `vram_estimate_mb` from the worker → bundle file sizes × 1.15 → "unknown". Free VRAM: worker data → the worker's `nvidia-smi` snapshot from `/api/image/capabilities` → `available_vram_mb` of the text neighbour on the same host.

The gate blocks **exactly one** meaningful case: the estimate is known, free VRAM is known, and the model does not fit. The answer is `503` with code `insufficient_vram`, a message, and a **hint listing the memory-reduction ladder**: lower the quantization → `--diffusion-fa` → text encoder on CPU (`--backend te=cpu`) → `--vae-tiling` → `--vae-conv-direct` → `--taesd` → `--offload-to-cpu` → lower resolution.

The lock is keyed by backend host, and by "host + GPU" when the backend declares `gpuIndex` (a backend field; for llama.cpp it falls back to `cppWorkerConfig.mainGpu`). This matters on multi-GPU machines: a card busy with generation no longer blocks text traffic going to another card on the same host. If only one side declares an index, the conflict is treated as host-wide (conservative).

Waiting for a text request blocked by generation is capped by `queueWaitTimeoutSec` (not by the shared admission queue): after the cap the client gets the standard `503` with `Retry-After`, the `X-Queue-Wait-Reason: image_gpu_lock` header and a `waitReason` field in the body.

## 6. Discovery: ask instead of reading the source

| Endpoint | What it returns |
|---|---|
| `GET /api/v1/image/contract` | the "decoded" contract: actual ports, endpoint list, request fields with normalization rules, limits, available models with defaults, curl examples, the **JSON Schema of the `generate_image` tool**, and instructions for an LLM agent (which URL to call, that the image arrives as `b64_json`) |
| `GET /api/v1/image/capabilities` | aggregate over all healthy image backends: samplers/schedulers/loras/upscalers (union), conservative limit merge (`min_*` = max, `max_*` = min, queues = sum), models per backend, and a report of probe failures |

Both require `X-API-Token` (port 18081). The aggregate is cached for 8 s and the cache is invalidated whenever the backend set or statuses change.

## 7. Ports and environment variables

| Variable | Default | Meaning |
|---|---|---|
| `LB_OPENAI_PORT` | 18079 | the balancer's OpenAI surface; a negative value disables the listener |
| `SDWORKER_PORT` | 18093 | image worker port |
| `SDWORKER_SD_SERVER_BIN` | `sd-server` | path to the engine binary |
| `SDWORKER_IMAGE_MODELS_DIR` | `models/image` | image model directory |
| `LB_ALLOW_IMAGE_TIMEOUT_SEC` | 0 (no cap) | opt-in cap for image generation |

## 8. Engine limitations (set expectations)

- **One model per process.** Switching models means restarting `sd-server` (the worker's `load`/`unload` do exactly that).
- **Generation is serialized** by a single mutex: concurrent requests queue up.
- **No in-flight cancellation and no per-step progress** (the C API has the primitives, the server does not expose them).
- **`city96/*` FLUX GGUF files do not load** in sd.cpp (that is the ComfyUI-GGUF format) — use `leejet/*` builds.
- **`/v1/images/variations` is implemented on top of img2img** (empty prompt + `strength` 0.5): sd.cpp has no dedicated variations mode, and the engine's behaviour with an empty prompt has not been verified against real sd.cpp (no engine binary in the test environment) — if it refuses, the client receives that error verbatim.
- **`--vae-on-cpu` costs ~5×** — prefer `--vae-tiling`; on AMD/RADV tiling is mandatory.
- sd.cpp releases several times a day and flag names have changed (`--host/--port` → `--listen-ip/--listen-port`) — the version is pinned in `types.PinnedSDServerRevision`.

## 9. Diagnostics

```bash
# image backend health
curl -s http://localhost:18081/api/v1/backends | grep -i image_cpp

# engine capabilities and limits (worker directly; balancer-side aggregation is /api/v1/image/capabilities)
curl -s http://<worker>:18093/api/image/capabilities

# model list (must contain sd-cpp-local and dall-*)
curl -s http://localhost:18079/v1/models

# contract and cluster capabilities (X-API-Token required)
curl -s -H "X-API-Token: $LB_TOKEN" http://localhost:18081/api/v1/image/contract | head -c 400
curl -s -H "X-API-Token: $LB_TOKEN" http://localhost:18081/api/v1/image/capabilities | head -c 400

# generate directly
curl -s -X POST http://localhost:18079/v1/images/generations \
  -H 'Content-Type: application/json' \
  -d '{"model":"dall-e-2","prompt":"a cat","size":"512x512","n":1}' | head -c 200
```

If `/v1/images/generations` answers `503 image_backend_unavailable`, no healthy `image_cpp` backend is registered. A `503 insufficient_vram` means the model does not fit the currently free VRAM — see the `hint` field in the response.

## 10. End-to-end smoke stand

`scripts/image-e2e-smoke.ps1` builds a mock `sd-server` (`tools/mock-sdserver`), the `sdworker` and the balancer, starts them as real processes and runs 17 checks (engine spawn, load/unload, seed normalization on the wire, CORS/`OPTIONS`, full path through port 18079, text isolation, A1111 stubs, health probe). It shifts busy ports automatically and kills every process in `finally`.

```powershell
powershell -ExecutionPolicy Bypass -File scripts/image-e2e-smoke.ps1
```
