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

**What is actually compared against free VRAM (important).** When the model is already loaded — which is precisely when the gate runs, weights being resident — the gate compares the **generation working set** (20% of the weights, no less than 256 MB and no more than 1 GB), not the full model weight: the weights are already held by the model itself, and demanding them again would reject generation on any card where less than the model size is free after loading. The "do the weights fit" question belongs to model-load time. This was fixed after a live run: SD1.5 Q4 (2.6 GB) on a card with 895 MB free answered `503` to every request while the engine was perfectly ready.

The lock is keyed by backend host, and by "host + GPU" when the backend's `gpuIndex` is **known**. This matters on multi-GPU machines: a card busy with generation no longer blocks text traffic going to another card on the same host. If only one side declares an index, the conflict is treated as host-wide (conservative).

`gpuIndex` is a backend field with **three states**, not two (a plain `int` cannot express them: `0` is a valid first-card index and is indistinguishable from "unset"):

| Field value | Meaning | Lock key |
|---|---|---|
| absent / `null` | index **unknown**: we do not know which card the backend uses | `host` (whole host) |
| `0` | **explicitly the first card** | `host#gpu0` |
| `N > 0` | explicitly card N | `host#gpuN` |

- `PUT /api/v1/backends/{id}` distinguishes all three: no key in the body → "do not change" (a partial PUT from the WebUI must not wipe the setting), `"gpuIndex": null` → **reset** to unknown, `"gpuIndex": 0` → explicit first card. A negative index → `400`.
- `POST /api/v1/backends` accepts the same field (including `0`); a missing key means "unknown".
- `GET /api/v1/backends` returns `gpuIndex` as a **number** when the index is explicit and **omits the field** when the index is unknown, so consumers can tell the difference.
- `gpuLockHeldFor` (whether a text request waits for generation) follows the same rule: an unknown index on either side means a host-wide lock.
- For llama.cpp, when no explicit `gpuIndex` is set the index falls back to `cppWorkerConfig.mainGpu`, but **only when `mainGpu > 0`**: zero there means "auto/unset", and treating it as "explicitly card 0" would narrow the lock on a guess.

Waiting for a text request blocked by generation is capped by `queueWaitTimeoutSec` (not by the shared admission queue): after the cap the client gets the standard `503` with `Retry-After`, the `X-Queue-Wait-Reason: image_gpu_lock` header and a `waitReason` field in the body.

## 6. Discovery: ask instead of reading the source

| Endpoint | What it returns |
|---|---|
| `GET /api/v1/image/contract` | the "decoded" contract: actual ports, endpoint list, request fields with normalization rules, limits, available models with defaults, curl examples, the **JSON Schema of the `generate_image` tool**, and instructions for an LLM agent (which URL to call, that the image arrives as `b64_json`) |
| `GET /api/v1/image/capabilities` | aggregate over all healthy image backends: samplers/schedulers/loras/upscalers (union), conservative limit merge (`min_*` = max, `max_*` = min, queues = sum), models per backend, and a report of probe failures |

**Where the image backend appears in general monitoring:** `GET /api/v1/metrics`
and `GET /api/v1/cluster` expose `backendType: "image_cpp"` with the `imagePort`
field (worker port) and an `image` block containing the worker state, the current
model, every known model (`models[]`: name, state, family, size, VRAM estimate,
active queries) and host VRAM. The WebUI (Backends/Dashboard/Monitor/Models) renders
this data, including the 🎨 image.cpp badge.

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

### 8.1 Which files you see in search, and what actually loads

The file list on the **HuggingFace** tab is NOT "the whole repository" — it is a
filtered set of **weights**: the worker only returns the
`ModelWeightExtensions = .gguf, .safetensors, .sft, .ckpt` extensions
(`internal/cppbackend/hf_bundle.go`), so `model_index.json`, README files and
scheduler configs never show up. On bundle start the extension is validated again
(`validateBundleRequest`) — an arbitrary file cannot be slipped in.

**`.safetensors` is recognized, not merely listed:**

- the file role is inferred from the name by the server-side heuristic
  (`internal/sdbackend/models.go` → `roleFromFilename`, exposed to the UI as
  `suggestedRole`): `ae.safetensors` → `vae`, `clip_l.safetensors` → `clip_l`,
  `clip_g.safetensors` → `clip_g`, `t5xxl*.safetensors/.gguf` → `t5xxl`,
  `*taesd*` → `taesd`, everything else (including `flux1-schnell.safetensors`) →
  `diffusion`. The **directory** counts too: `vae/...` → `vae` and
  `text_encoders/qwen3vl_8b_bf16.safetensors` → `llm` (the Qwen-Image LLM encoder),
  while `qwen-image-2.1-UC-Q4_0.gguf` in the repository root stays `diffusion` —
  the LLM rule only applies inside an encoder directory;
- the file list is **recursive** (`tree API?recursive=true`): DiT repositories keep
  their VAE and text encoder in subdirectories (`vae/`, `text_encoders/`,
  `split_files/`), and before 2026-10-03 such files never reached the UI, so a DiT
  bundle could not be assembled at all. HF pages the response, so pages are followed
  via the `cursor` from the `Link` header (without leaving the configured mirror);
  files are downloaded by their original path and stored under their base name;
- the engine loads safetensors natively, because sd.cpp runs on ggml: an
  all-in-one `.ckpt/.safetensors/.gguf` is passed via `--model`, separate files go
  through `--vae` (`ae.safetensors`), `--clip_l/--clip_g`, `--t5xxl`, `--taesd`,
  `--llm` (see `docs/research-sdcpp-lowvram-integration.md`, the file layout
  section);
- in practice: SD 1.5/SDXL need a single all-in-one `.safetensors`, while
  DiT families (FLUX/SD3/Qwen-Image/Z-Image) need a set of `diffusion` (usually a
  GGUF from `leejet/*`) + `ae.safetensors` + text encoders.

Caveat: `.sft` passes the file filter (a legacy of the shared HF helper), but
sd.cpp support for `.sft` specifically has not been verified — treat it as
at-your-own-risk and prefer `.gguf`/`.safetensors`.

### 8.2 How the engine identifies a model version (and where "get sd version from file failed" comes from)

The engine does NOT read `general.architecture`: it derives the model version from
**tensor names** (`ModelLoader::get_sd_version`, `src/model_loader.cpp` of the
pinned `master-929-3f8527a`). And those names depend on the flag the file is
attached with (`src/pipeline/diffusion_engine.cpp`:726 and :733):

| flag | effect on tensor names | for |
| --- | --- | --- |
| `--diffusion-model f.gguf` | prepends `model.diffusion_model.` | DiT: FLUX/FLUX2/SD3/Qwen-Image/Z-Image/Chroma |
| `--model f.gguf` | keeps the names as stored in the file | all-in-one: SD1.x/SD2.x/SDXL |

Both traps we have already hit follow from this:

- **A DiT file attached as all-in-one.** Bare names (`transformer_blocks.*`,
  `double_blocks.*`) are unrecognisable in that mode → `get sd version from file
  failed` after gigabytes were downloaded. The fix is the profile family: for DiT
  families the worker passes `--diffusion-model` itself (`pkg/types/image_model.go`,
  `IsDiTFamily`).
- **"sd.cpp cannot read ComfyUI builds" is wrong.** Verified on the
  `master-929-3f8527a` engine with synthetic GGUF files carrying real HF tensor
  names: both the `leejet/*` build (fused `img_mlp.gate_up`) and the ComfyUI export
  (split `img_mlp.gate_layer` + `img_mlp.proj`,
  `general.architecture="qwen_image21"`) report `Version: Qwen Image 2.1` — the
  engine supports both layouts (`src/model/diffusion/qwen_image_2_1.hpp`).

**UI marks (so this is not discovered at load time).** The **HuggingFace** tab
pre-checks the file header: the Check button next to a file (and automatically for
the largest file right after a repository is picked) calls
`GET /api/hf/probe?modelId=&filename=&revision=`. The worker reads the first
512 KB with a Range request (`internal/cppbackend/hf_probe.go`, not a single byte
of weights) and answers with facts:

- `verdict=supported` + `family` + `versionLabel` — which family and which engine
  version it recognises in the file (e.g. `qwen_image` / "Qwen Image 2.1");
- `dit=true` — the file is attached as `--diffusion-model`, so the profile family
  must be a DiT family. The UI sets it automatically unless the operator picked a
  family by hand, and warns separately on a mismatch ("the profile says `other`,
  the engine will answer get sd version from file failed");
- `verdict=unknown` — not identifiable from the header (VAE, text encoder, LoRA or
  an unknown family). The pre-check never issues "the engine cannot read it"
  verdicts: 512 KB of header do not contain everything the engine needs.

### 8.3 Other limitations

- **One model per process.** Switching models means restarting `sd-server` (the worker's `load`/`unload` do exactly that).
- **Generation is serialized** by a single mutex: concurrent requests queue up.
- **No in-flight cancellation and no per-step progress** (the C API has the primitives, the server does not expose them).
- **The tensor layout matters more than who built the file.** `city96/*`,
  `unsloth/*` and other ComfyUI-oriented repositories do load when their tensor set
  matches what the engine expects (see 8.2) — use the Check button rather than the
  author name. An unfamiliar layout (for example a different implementation of the
  same family) yields `get sd version from file failed`.
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

## 10. Live test stand

`scripts/image-e2e-smoke.ps1` builds a mock `sd-server` (`tools/mock-sdserver`), `sdworker` and the balancer, starts them as real processes and runs 21 checks. It shifts busy ports automatically and kills every process in `finally`.

```powershell
# protocol run on the mock engine (seconds)
powershell -ExecutionPolicy Bypass -File scripts/image-e2e-smoke.ps1

# run on the REAL engine and a real model (Vulkan/CPU; minutes on CPU)
powershell -ExecutionPolicy Bypass -File scripts/image-e2e-smoke.ps1 -Real
```

With `-Real` the engine and the model are downloaded automatically if missing:
`tools/fetch-sdcpp` fetches `stable-diffusion.cpp` release assets from GitHub and
files from HuggingFace (Range resume, retries, `HF_TOKEN`):

```powershell
go run ./tools/fetch-sdcpp list-release leejet/stable-diffusion.cpp win-vulkan
go run ./tools/fetch-sdcpp get-release  leejet/stable-diffusion.cpp win-vulkan .\bin\sdcpp
go run ./tools/fetch-sdcpp hf-get   second-state/stable-diffusion-v1-5-GGUF stable-diffusion-v1-5-pruned-emaonly-Q4_0.gguf .\bin\hf-models
```

What `-Real` proves that the mock cannot: real `sd.cpp` accepts our requests, and
**the image obtained through the balancer is a valid PNG of the expected size**
(check B8; the file is saved into the stand working directory). Checks that rely on
the mock's log (seed normalization, `init_image` for img2img) are marked `SKIP` in
this mode.

The same stand runs in CI (job `test-self-hosted`, step "Image chain E2E"); the
in-process version of the client scenarios runs on the ubuntu fallback
(`go test -run TestImageSmoke ./tests/`).

## 11. Docker

The image worker image is built from an `sd.cpp` release (Vulkan is the default
path; the project publishes no Linux-CUDA release, CUDA is wired through your own
binary in `docker/imageworker/vendor/`).

```bash
# bundled-full: the image worker starts BY DEFAULT (no profiles)
docker compose -f deployments/docker-compose.bundled-full.yml up -d --build

# stack: enable the image worker with the worker|full profile
docker compose -f deployments/docker-compose.stack.yml --profile full up -d --build imageworker

# image worker only (self-contained file)
docker compose -f deployments/docker-compose.imageworker.yml --profile vulkan up -d

# disable
docker compose -f deployments/docker-compose.stack.yml --profile full rm -sf imageworker
```

The balancer publishes the OpenAI surface (`18079`) — that is where clients send
requests: `POST http://<host>:18079/v1/images/generations`. To confirm the backend
registered as an image backend:

```bash
curl -H "X-API-Token: $TOKEN" http://<host>:18081/api/v1/image/backends
```

NVIDIA requires nvidia-container-runtime with `NVIDIA_DRIVER_CAPABILITIES=compute,utility,graphics`
(without `graphics` no Vulkan ICD is mounted into the container); AMD/Intel need
`/dev/dri` plus `group_add: video,render` (see `docker-compose.imageworker.yml`).

### 11.1 Single stand: text and images behind one balancer

The `full` profile of `deployments/docker-compose.stack.yml` brings up **both**
backend pools behind one balancer:

```bash
cd deployments
docker compose -f docker-compose.stack.yml --profile full up -d --build
```

| Service | Backend type | Port | Role |
|---|---|---|---|
| `loadbalancer` | — | 18080 / 18081 / **18079** | entry point, admin API, image OpenAI surface |
| `webui` | — | 18083 | operator UI (including the "Image backends" page) |
| `cppworker-gpu` | `llama_cpp` | 18092 | text |
| `imageworker` | `image_cpp` | 18093 | image generation (sd.cpp) |
| `agent` | — | 18032 | GPU/RAM metrics and text backend registration |

Both workers register themselves with the balancer (label `auto-registered`), so
there is no need for two balancers: routing follows the request type (text →
`llama_cpp`, `/v1/images/*` and `/sdapi/v1/*` → `image_cpp`). The model directory
is shared: `${MODELS_DIR}/image/<name>/` holds image bundles, `${MODELS_DIR}/*.gguf`
holds text models.

What is handled specifically for the single stand:

- `imageworker` waits for a **ready** balancer (`depends_on: condition: service_healthy`);
  otherwise its first registration POST hits a closed port;
- **re-registration with a changed address** updates the record instead of a
  permanent `409`: a recreated container (new host/port) no longer leaves a dead
  record behind (see `isReRegistrationOfAutoBackend`);
- image model profiles are stored in `/app/data/image-model-profiles.json`
  (`LB_IMAGE_MODEL_PROFILES_PATH`) because `../config` is mounted `:ro` — saving a
  profile from the WebUI would otherwise fail on a read-only filesystem;
- stand smoke test: `powershell -File scripts\docker-stack-smoke.ps1` (checks the
  profile composition, both backend types, `cluster.image`, the WebUI page, and
  with `-Generate` a real generation plus growing metrics).

```bash
# what the balancer actually sees after startup
curl -s -H "X-API-Token: $TOKEN" http://<host>:18081/api/v1/backends | jq '.backends[] | {id, type, status}'
curl -s -H "X-API-Token: $TOKEN" http://<host>:18081/api/v1/cluster  | jq '.cluster.image.requests'
```

**About building the images.** In `docker-compose.stack.yml` the `build:` section
exists for `loadbalancer`, `webui` and `imageworker`, so
`--profile full up -d --build` rebuilds exactly those; `cppworker-gpu` and `agent`
come from images (their code did not change in this phase, and rebuilding llama.cpp
takes tens of minutes). Without `build:` on the balancer and WebUI, `--build`
rebuilt only the image worker while the balancer/UI stayed on old images — which is
exactly what looks like "the new backend is not active in Docker" and "the WebUI
does not see the image backend".

**About the WebUI `?v=` token.** The Dockerfile replaces the `?v=` token in all
html files with `WEBUI_VERSION` (build arg), and nginx serves js with
`expires 1y`. The compose default is `0.7.0`; with an unchanged token the
operator's browser keeps the OLD modules from cache even after a rebuild. Bump
`WEBUI_VERSION` when you ship new frontend code.

#### GPU inside the container: verify, do not assume

`nvidia-smi` inside the container sees the card (compute/utility are passed
through), but sd.cpp needs **Vulkan**, and the NVIDIA ICD only reaches the
container with the `graphics` capability and a Vulkan ICD in the host driver
store:

```bash
docker exec ol-stack-imageworker sh -c 'ls /usr/share/vulkan/icd.d/'
# no nvidia_icd.json  -> NVIDIA Vulkan is unavailable in the container
docker exec ol-stack-imageworker nvidia-smi -L   # GPU present (CUDA), but not Vulkan
```

Measured on the live stand (Windows + Docker Desktop/WSL2, RTX 3070): inside the
container 512x512 / 8 steps took **251 s** (the engine falls back to CPU/software
Vulkan), while the native `sd-server` with Vulkan on the same host took **9-76 s**.

**Solution: build sd.cpp with CUDA right in the image.** The project publishes no
Linux CUDA release, so `docker/imageworker/Dockerfile` can compile the engine from
source, and the variant is selected by the build TARGET:

| Target | Engine | When it is needed |
|---|---|---|
| `imageworker-vulkan` (default) | the released Vulkan asset | Linux hosts with an nvidia ICD, AMD/Intel |
| `imageworker-cuda` | sd.cpp built with CUDA (`-DSD_CUDA=ON`) | where NVIDIA Vulkan never reaches the container (Windows + Docker Desktop/WSL2) |

```bash
# deployments/.env
IMAGE_WORKER_BUILD_TARGET=imageworker-cuda
IMAGE_WORKER_RUNTIME_BASE=dockerhub.timeweb.cloud/nvidia/cuda:12.2.0-runtime-ubuntu22.04
IMAGE_WORKER_CUDA_ARCH=86          # compute capability: 86 = RTX 30xx
IMAGE_WORKER_TAG=cuda12            # local build; a release (release-all.ps1) writes the release tag here

cd deployments
docker compose -f docker-compose.stack.yml --profile full build imageworker
docker compose -f docker-compose.stack.yml --profile full up -d
```

On a release, `scripts/release-all.ps1` changes `IMAGE_WORKER_TAG` (service
`imageworker`): it builds the image through the compose target and writes the
release tag into `deployments/.env` — the same tag the balancer and WebUI get.
The old image name (`:cuda12`) stays in the local daemon as the previous
version, so rolling back means restoring the previous `IMAGE_WORKER_TAG` value and
running `docker compose ... up -d`.

Why CUDA 12.2 and ubuntu 22.04: there is no Linux CUDA build of sd.cpp, and
`nvidia/cuda:12.2.0-devel/runtime-ubuntu22.04` is already in the local cache (the
text `cppworker` is built on it), so the build does not pull a multi-gigabyte
image. The binary is built on the devel base and runs on the runtime base of the
same version; `SD_BUILD_SHARED_LIBS=OFF` (the sd.cpp default) produces a static
ggml/stable-diffusion, so the runtime needs only the CUDA runtime and
libstdc++/libgomp.

Alternative when you would rather not build: **a native image worker on the host +
the same balancer** — run `sdworker` outside Docker with
`SDWORKER_BALANCER_URL=http://<host>:18081`,
`SDWORKER_BALANCER_TOKEN=<stack token>`,
`SDWORKER_ADVERTISE_HOST=host.docker.internal` and
`SDWORKER_REGISTER_DISABLE=true` on the containerized worker — then the balancer
(in Docker) proxies images to the native worker with GPU Vulkan.

## 12. Image request metrics and backend management in the UI

### 12.1 What is counted

The balancer counts **generation** requests: `POST /v1/images/*`,
`POST /sdapi/v1/txt2img|img2img`, `POST /api/image/generate`. Management calls
(model list, `load`/`unload`, `capabilities`) are NOT counted — otherwise RPS and
"average time" would show service traffic instead of generation.

There are five outcomes, and they mean different things:

| Status | What happened |
|---|---|
| `ok` | a synchronous generation returned an image |
| `failed` | the request reached the worker/engine and failed (5xx, broken connection) |
| `rejected` | the balancer gate refused it (no model, not enough VRAM, GPU lock held, queue timeout, no backend) |
| `accepted` | asynchronous submission (`202`), generation still running |
| `finished` | asynchronous generation finished (the balancer does not track per-job results — the worker only exposes `active_queries`) |

### 12.2 Where to look

- `GET /api/v1/metrics` (requires `X-API-Token`) → the `image` block:
  `requests` (pool aggregate plus `recent`, the shared feed) and
  `backends[<id>].requests` (counters of one worker);
- `GET /api/v1/cluster` → `cluster.image` (same aggregate and feed) plus
  `cluster.backends[<id>].image.requests` (per backend). This is what the Monitor
  reads, so the requests panel needs no extra poll per tick;
- `GET /api/v1/image/backends/{id}/models` → the worker's model state.

Aggregate fields: `inFlight`, `total` (requests started), `ok`, `failed`,
`rejected`, `accepted`, `finished`, `rps` (60 s window), `avgDurationMs`,
`p50DurationMs`, `p95DurationMs`, `lastDurationMs`, `lastRequestAt`,
`failuresByCode` (engine error codes), `gateDeniedByCode` (gate refusal reasons).
Memory is hard-capped: a 20-entry feed per backend and 50 shared entries, with
512 duration samples.

### 12.3 UI

The WebUI part is a single **"Image models"** page (`#image-page`) built like the
"GGUF models" page: the nav item appears once the cluster has at least one
`image_cpp` backend, and it contains six tabs.

| Tab | What it does |
|---|---|
| **Overview** | Image backend CRUD, worker port, GPU index, state, current model, VRAM, request counters, text/image coexistence policy; the **"Backend check"** button loads a model first if none is loaded and then runs 1 step at 64x64 over the client path `POST /v1/images/generations`; only the result, the time and the model name reach the UI, no image is displayed |
| **HuggingFace** | Repository search (query + `text-to-image` filter), file list with suggested roles, selection and role override, bundle name/family, `HF token`, "Download bundle". Every file has a header pre-check ("Check"): which family the engine recognises and whether `--diffusion-model` is required; the profile family is auto-filled from the file (see 8.2) |
| **Models on disk** | Bundle table: name, state, size, family, **contents by role**, active queries, VRAM estimate; actions — load, unload, **delete from disk** |
| **Loaded** | Worker state and current model plus load progress (stage, time) over SSE `/api/image/models/load/progress/stream` with a polling fallback |
| **Downloads** | Active bundle and single-file downloads, history, residual `.download` files with cleanup, cancel |
| **Settings** | Parameters of the selected backend (entry into its card) and image model profiles (`image-profiles.js` editor) |

Why it works this way:

- **Showing generated images in the WebUI is gone.** The WebUI is a panel for
  configuring the balancer and understanding system state; displaying results is the
  clients' job (OpenAI/A1111 on `:18079`), which have previews and their own history.
  The generation form, "Result" and the localStorage gallery were removed; the
  "Backend check" button (1 step at 64x64, no image) remains — it answers "is the
  engine alive?", it does not replace a client.
- **File roles are computed by the server.** In the repository file list the worker
  returns `suggestedRole` (`internal/sdbackend.SuggestRole`); the UI only displays it
  and lets the operator override it. If the "which file is a VAE" heuristic lived in
  JS, the rules would drift from the worker and bundles would be assembled wrongly.
- **Auto-selection is conservative.** `diffusion`, `vae`, `clip_l`, `clip_g` are
  selected by default; `t5xxl`/`llm` (3-9 GB) must be picked by hand.
- **Bundle deletion** goes through `POST /api/image/models/delete`: only inside
  `ModelsDir`, a loaded bundle returns `409` with an `unload` hint (the engine keeps
  the weights open), and the worker registry is reloaded after the delete.
- **Monitor** stays the second observation window: the "Image backend requests" panel
  (aggregates plus a feed of the latest requests with path, model, size, steps,
  duration and status), and in the backends table the Active/RPS/Avg RT columns of an
  `image_cpp` backend are filled from `image.requests`.

### 12.4 "Test" tab - try model settings and see the result

The seventh tab of the "Image models" page (the "Test" button), available
**only when the cluster has an `image_cpp` backend** - like the whole page.

WHY A SEPARATE TAB: the other tabs are about configuration and state, and showing
generated images there is deliberately excluded (see 12.3). The "Test" tab is the
opposite: it is the only WebUI place that displays an image, and it does not get in
the way until opened.

What it does:

- pick a backend (`image_cpp`) and a model, buttons "Load/Unload model",
  "From profile" - fill the form with the model profile defaults
  (steps/cfg/sampler/size);
- pick the request surface: OpenAI `/v1/images/generations`, A1111
  `/sdapi/v1/txt2img`, native `/api/image/generate`; parameters - prompt,
  negative, width, height, steps, cfg, sampler, scheduler, seed, batch
  (limits come from `/api/image/capabilities`);
- a "What the request will contain" block with live JSON and curl: you can see
  which settings are applied instead of guessing from the picture;
- the result: image, duration, HTTP status, `model`, `seed`, size; run history is
  **in-memory (this tab only)** to compare settings, "To form" restores the
  parameters - no localStorage, no gallery;
- the request goes over the balancer **client path**, so it passes the VRAM gate
  and is counted in metrics: your own checks show up in Monitor;
- engine errors are explained: "get sd version from file failed" means the profile
  family does not match the file (a DiT model attached as all-in-one, see 8.2 — the
  family is auto-filled on the HuggingFace tab via the Check button); "no image
  model is loaded" means press "Load model"; OOM means reduce size/steps/batch or
  enable offload; "port still busy" means the engine is still releasing the socket.
