# OllamaLegion CppWorker: n_ctx, Per-Model Profiles, Ollama ↔ OpenAI Proxy

> **Version:** 2.0 (2026-06-22)  
> **Related documents:** [`api.md`](api.md), [`runbook-tools.md`](runbook-tools.md), [`audit-2026-06.md`](audit-2026-06.md), [`../.clinerules`](../.clinerules) §3–4  
> **Tests:** `cmd/cppworker/*_test.go`, `tests/nctx_reload_integration_test.go`, `tests/llamacpp_initial_setup_test.go`

## Table of Contents

1. [Per-Model Profiles and n_ctx](#1-per-model-profiles-and-n_ctx)
2. [3-tier resolver for n_ctx](#2-3-tier-resolver-for-n_ctx)
3. [Per-model profile (structure)](#3-per-model-profile-structure)
4. [Header `X-Cpp-Ctx`](#4-header-x-cpp-ctx)
5. [RAM fallback and reload](#5-ram-fallback-and-reload)
6. [Tools/tool_calls and RAM fallback](#6-toolstool_calls-and-ram-fallback)
7. [API endpoints (Per-Model Profiles + n_ctx-reload)](#7-api-endpoints-per-model-profiles--n_ctx-reload)
8. [WebUI setup wizard](#8-webui-setup-wizard)
9. [Ollama ↔ OpenAI translation](#9-ollama--openai-translation)
10. [Typical scenarios](#10-typical-scenarios)

---

## 1. Per-Model Profiles and n_ctx

### 1.1 The problem

`n_ctx` (context size) in llama.cpp is **immutable after model loading**. If a model is loaded with `n_ctx=4096`, you **cannot** increase it to 16384 without reloading — Cline/OpenWebUI will report `n_ctx overflow` when trying to use a long system prompt.

Previously, the administrator had to:
1. Change `LLAMA_CTX_SIZE` in cppworker's `.env`.
2. Restart the cppworker container (downtime ~10-30 seconds).
3. Hope that 8K was enough for all models.

This is bad because:
- **gemma-4-E4B-it-Q4_K_M** wants 32K-128K (large system prompts, long context).
- **llama-3.1-8b** works comfortably at 8K.
- **qwen2.5-coder-7b** needs 16K for file editing.
- **embedding models** do not need a long context.

### 1.2 The solution

**Per-model profile** + **3-tier resolver** + **reload endpoint** provide:
- Setting `n_ctx` for each model independently.
- Applying a new profile **on the fly** (reload without restarting the container).
- The client can override via `options.num_ctx` in the body (Ollama) or `num_ctx` (OpenAI).

---

## 2. 3-tier resolver for n_ctx

File: `internal/balancer/num_ctx_resolver.go`. When handling each request, the balancer computes the effective `n_ctx`:

```
┌────────────────────────────────────────────────────────────────────┐
│ Tier 1: request body (highest priority)                             │
│   Ollama:   {"options": {"num_ctx": 4096}}                         │
│   OpenAI:   {"num_ctx": 4096}                                      │
│   → the client explicitly requested it → this always wins          │
├────────────────────────────────────────────────────────────────────┤
│ Tier 2: per-model profile (config.LlamaCppModelProfiles[name])     │
│   → if the admin configured a profile for gemma-4 → 32768          │
├────────────────────────────────────────────────────────────────────┤
│ Tier 3: per-backend default (state.Backend.CppWorkerConfig.ContextLength) │
│   → fallback from the cppworker default (LLAMA_CTX_SIZE from .env)  │
└────────────────────────────────────────────────────────────────────┘
```

**Important:**
- Tier 1 always wins.
- Tier 2 beats Tier 3 — the model profile takes priority over the backend default.
- If none of the tiers provide a value (0), cppworker uses its own `defaultCtxSize`.
- In `config/config.json` **`defaultModelProfile.contextLength` must be `0` or absent** (otherwise Tier 2 fixes the value and Tier 3 is never reached).

After resolution, the balancer sets the `X-Cpp-Ctx` header in the request to cppworker (if `Value > 0`).

---

## 3. Per-model profile (structure)

The profile lives in `config.json` under the `llamaCppModelProfiles` key:

```json
{
  "llamaCppModelProfiles": {
    "gemma-4-E4B-it-Q4_K_M": {
      "contextLength": 32768,
      "batchSize": 1024,
      "numGpuLayers": -1,
      "flashAttn": true,
      "numa": false,
      "useMmap": true,
      "notes": "Cline long context + system prompt"
    },
    "llama-3.1-8b-instruct-q4_K_M": {
      "contextLength": 8192,
      "batchSize": 512,
      "numGpuLayers": -1
    }
  }
}
```

### 3.1 Profile fields

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `contextLength` | int | **yes** | — | Context size. Range `[256, 262144]` (256K). |
| `batchSize` | int | no | 512 | Batch size. `0` = not set. |
| `numGpuLayers` | int | no | -1 | GPU layers. `-1` = all, `0` = CPU-only, `N` = specific number. |
| `flashAttn` | bool (ptr) | no | true | Flash Attention. `null` = do not override. |
| `numa` | bool (ptr) | no | false | NUMA optimization. |
| `useMmap` | bool (ptr) | no | true | Memory mapping. |
| `notes` | string | no | "" | Free-form comment for the admin. |

> **Why are bool fields pointers?** PATCH semantics: if the body has `flashAttn: false`, it will overwrite `true` in the profile. If the field is absent — the old value is preserved.

### 3.2 Validation

- `contextLength` ∈ `[256, 262144]`. 256K is the ceiling for gemma-4 (native 262144).
- `contextLength = 0` in a profile is **not allowed**.
- `batchSize < 1` — error (if set).
- `numGpuLayers < -1` — error.

---

## 4. Header `X-Cpp-Ctx`

When the balancer resolves `n_ctx > 0` from a profile or the backend default, it adds to the request to cppworker the header:

```
X-Cpp-Ctx: 32768
```

cppworker, in `cmd/cppworker/main.go:applyCppCtxHeader`, reads this header **and** `body.options.num_ctx` (Ollama) / `body.num_ctx` (OpenAI), and applies it to `params.NCtxOverride`.

**Priority inside cppworker (within a single request):**

```
1. body.options.num_ctx (Ollama) / body.num_ctx (OpenAI) — highest
2. X-Cpp-Ctx header from balancer — fallback
3. defaultCtxSize (from LLAMA_CTX_SIZE) — last fallback
```

> **Body > Header.** This rule is general for both levels (balancer→cppworker, and inside cppworker). The body is an explicit per-request override, the header is policy.

**Header semantics: upper limit.**
- If `body.num_ctx <= headerLimit` → the header does not lower it.
- If `body.num_ctx > headerLimit` → clamp to headerLimit.

Additionally:
- If the body has `num_predict` and its default exceeds header/2 — `clampNPredictToFitContext` is performed (see `internal/balancer/nctx_clamp.go`).

---

## 5. RAM fallback and reload

### 5.1 RAM fallback

`cmd/cppworker/inference.go:tryRamFallbackReload` (around line 491):

When a model requests an `n_ctx` that exceeds VRAM, cppworker tries to:
1. Check the `ramFallbackNCtx` flag (env `CPPWORKER_RAM_FALLBACK_N_CTX=true` or `--ram-fallback-n-ctx`).
2. Compare with `ramFallbackMaxNCtx` (env `CPPWORKER_RAM_FALLBACK_MAX_N_CTX`, default no limit).
3. `UnloadModel()` the current model.
4. `LoadModelWithOpts(n_ctx=requestedNCtx, use_mmap=true, gpu_layers=ramFallbackGpuLayers)`.
5. On success — retry inference.

**`ramFallbackAttempts` counter:** limited to `ramFallbackMaxAttempts=3` per 60-second window. Exceeding it → `ReloadLoopLimitError` → HTTP 413.

### 5.2 Auto-reload on the balancer

`internal/balancer/nctx_reload.go:NCtxReloadCoordinator`:

When cppworker returns `bridge code 2` (`ErrNCtxNeedsReload`), the balancer:
1. Parses `NCtxError` via `internal/balancer/llamacpp_error.go:ParseCppWorkerError`.
2. Calls `DecideReloadBackend(model, requestedNCtx, maxVramCtx)`.
3. The decision is based on `NCtxReloadConfig` (`AutoReloadNCtx`, `VramSafetyFactor`, `MaxNCtx`).
4. If the decision = `DecisionAccept` → `POST /api/models/reload` to cppworker.

### 5.3 Endpoint `POST /api/models/reload` (cppworker)

Applies new parameters to the loaded model:

```json
POST /api/models/reload
Content-Type: application/json

{
  "name": "gemma-4-E4B-it-Q4_K_M",
  "contextSize": 32768,
  "batchSize": 1024,
  "numGpuLayers": -1,
  "flashAttn": true
}
```

What it does:
1. `unload` the current model (if loaded).
2. `load` with new parameters (via the C-bridge `bridge_load_model`).
3. Pre-flight check (`bridge_check_ctx_capacity`) before loading.
4. Returns `200 OK` after successful loading.

**Reload time:** 5-30 seconds depending on model size and disk.

### 5.4 Protection against double loading

`cmd/cppworker/handlers_model.go:handleLoadModel`:
- Before `LoadModelWithOpts`, it checks `backend.GetModel(modelName)`.
- If the model is **already loaded** with the same path/parameters → returns `status: "already_loaded"`.
- If parameters or path differ → `UnloadModel` + `LoadModelWithOpts` (reload-in-place).
- Race condition protection via mutex + `WaitForLoad`.

---

## 6. Tools/tool_calls and RAM fallback

`cmd/cppworker/inference.go:tryRamFallbackReload` **disables reload if the request has `tools[]`** (`hasTools=true`):

```go
if hasTools {
    return ReloadDisabledForToolsError
}
```

`cmd/cppworker/utils.go:writeReloadDisabledForToolsResponse` translates this into **HTTP 413** with a hint:
- Reduce `tools[]`/history.
- Or increase `n_ctx`.

**Why:** `tools[]` is serialized into the system prompt — after a reload, the context would be occupied by tools, and the inference would not fit.

---

## 7. API endpoints

### 7.1 Per-Model Profiles (`internal/api/handlers_cppworker_profiles.go`)

All endpoints are protected by `AuthMiddleware` (if `API_TOKEN` is set) and `RateLimitMiddleware`.

#### `GET /api/v1/cppworker/model-profiles`

List of all profiles.

**Response 200:**
```json
{
  "models": {
    "gemma-4-E4B-it-Q4_K_M": {
      "contextLength": 32768,
      "batchSize": 1024,
      "numGpuLayers": -1,
      "flashAttn": true,
      "numa": false,
      "useMmap": true,
      "notes": "Cline long context"
    }
  },
  "total": 1
}
```

#### `GET /api/v1/cppworker/model-profiles/{name}`

Get the profile for a single model. `404` if there is no profile.

#### `PUT /api/v1/cppworker/model-profiles/{name}`

Create or update a profile. **Persisted in `config.json`.**

**Body:**
```json
{
  "contextLength": 32768,
  "batchSize": 1024,
  "numGpuLayers": -1,
  "flashAttn": true,
  "notes": "Cline long context"
}
```

**Response 200:** `{ "status": "ok", "model": "...", "profile": {...} }`

**Errors:** `400 invalid JSON`, `400 invalid profile`, `400 model name required`.

#### `DELETE /api/v1/cppworker/model-profiles/{name}`

Delete a profile. **Persisted in `config.json`.**

**Response 200:** `{ "status": "ok", "model": "..." }`

#### `POST /api/v1/cppworker/model-profiles/{name}/apply`

**The main endpoint for the admin:** apply the profile with reload on all backends.

**Body (optional):** patch — fields from the body are merged with the current profile (zero-value fields do not overwrite).

**Algorithm:**
1. Read the body, merge with the current profile.
2. Validate the merged profile.
3. Save to config.
4. For each llama_cpp backend:
   - If the model is **not loaded** → `skipped: "model not currently loaded on this backend"`.
   - If loaded → `POST /api/models/reload` to cppworker.
5. Return the aggregated result.

**Response 200:**
```json
{
  "model": "gemma-4-E4B-it-Q4_K_M",
  "profile": { "contextLength": 32768, "batchSize": 1024, "numGpuLayers": -1 },
  "backends": [
    { "backendId": "llama-gpu-1", "status": "reloaded" },
    { "backendId": "llama-cpu-1", "status": "skipped", "message": "model not currently loaded on this backend" }
  ]
}
```

### 7.2 n_ctx-reload endpoints (`internal/balancer/nctx_reload_handlers.go`)

#### `POST /api/v1/nctx-reload/{backendId}/reset`

Resets the `ramFallbackAttempts` counter on the balancer for a single backend.

**Usage:** after manually fixing the cause of the reload loop (for example, freeing up VRAM).

```bash
curl -X POST http://localhost:18081/api/v1/nctx-reload/cppworker-gpu-1/reset
```

#### `POST /api/v1/nctx-reload/reset`

Resets counters for all backends.

```bash
curl -X POST http://localhost:18081/api/v1/nctx-reload/reset
```

#### `GET /api/v1/nctx-reload/status`

Snapshot of the coordinator state: per-backend metrics (attempts, errors, last decision).

**Response 200:**
```json
{
  "backends": {
    "cppworker-gpu-1": {
      "attempts": 2,
      "decisions": { "accept": 1, "reject": 1, "noop": 0 },
      "lastError": "n_ctx overflow",
      "lastDecisionAt": "2026-06-22T12:00:00Z"
    }
  }
}
```

### 7.3 `POST /api/v1/cppworker/reset-reload-counter` (R-6 ✅)

> **Status:** ✅ implemented in `cmd/cppworker/handlers_reset_reload.go`.

When the `ramFallbackAttempts` counter on the cppworker side reaches the limit, the administrator can reset it without `docker restart`.

**Without body** — resets the counter for all models.

```bash
curl -X POST http://localhost:18091/api/v1/cppworker/reset-reload-counter
```

**With `{"model": "..."}`** — resets only for the specified model.

```bash
curl -X POST http://localhost:18091/api/v1/cppworker/reset-reload-counter \
  -H "Content-Type: application/json" \
  -d '{"model":"gemma-4-E4B-it-Q4_K_M"}'
```

**Response 200:**
```json
{
  "status": "ok",
  "model": "",
  "resetCount": 3,
  "message": "ramFallbackAttempts cleared. Cppworker can now attempt reload again."
}
```

**Protection:** `authMiddleware` (if `API_TOKEN` is set).

**Typical errors:**
- `405 Method Not Allowed` — used with a non-POST method.
- `400 Bad Request` — invalid JSON in the body.

**Tests:** `cmd/cppworker/handlers_reset_reload_test.go` (5 tests).

---

## 8. WebUI setup wizard

In **Settings → CppWorker → Model Profiles** a visual wizard is available:

1. **List of profiles** — all configured models.
2. **Add Profile** — wizard:
   - "Model name" field (required).
   - n_ctx slider from **256** to **262144** (256K) with presets: 4K, 8K, 16K, 32K, 64K, 128K, 256K, Custom.
   - `batchSize`, `numGpuLayers`, `notes` fields.
3. **Apply** — calls `POST /.../apply`, shows a **progress bar**:
   - Step 1: Save profile (HTTP PUT).
   - Step 2: Reload on backends (HTTP POST /apply).
   - Step 3: Done.
4. **Edit / Delete** — icons next to the profile.

Also in Settings → CppWorker, **Backend load options** for the selected cppworker are displayed:
- `defaultCtxSize`, `defaultBatchSize`, `defaultGpuLayers`, `defaultFlashAttnType`, `defaultNuma`, `defaultUseMmap`, `defaultNThreads`.
- Read-only: `nodeName`, `balancerUrl`, `uptime`.

> **i18n:** all strings are translated in `webui/js/i18n/ru.js` and `en.js` (key `settings.profiles.*`).

---

## 9. Ollama ↔ OpenAI translation

File: `internal/balancer/llamacpp_translate_*.go`.

The balancer translates the Ollama request format into the OpenAI format for cppworker and back.

### 9.1 Architecture

```
Client (OpenWebUI) → Balancer (ollamalegion) → CppWorker (llama.cpp)
     Ollama API           Format translation        OpenAI API
   /api/chat           →  /v1/chat/completions
   /api/generate       →  /v1/completions
   /api/embeddings     →  /v1/embeddings
   /api/tags           →  /v1/models (or metrics)
```

### 9.2 Field mapping

| Ollama | OpenAI |
|---|---|
| `options.temperature` | `temperature` |
| `options.top_p` | `top_p` |
| `options.top_k` | `top_k` |
| `options.num_predict` | `max_tokens` |
| `options.num_ctx` | `num_ctx` |
| `options.stop` (string or []string) | `stop` |
| `options.repeat_penalty` | `repeat_penalty` |
| `options.seed` (0 is valid) | `seed` |
| `prompt` | `prompt` (for `/v1/completions`) |
| `messages` | `messages` |
| `tools[]` | `tools[]` |
| `stream` | `stream` |

### 9.3 Streaming translation

`internal/balancer/llamacpp_translate_resp.go:translateOpenAISSEDataToOllama`:
- Converts OpenAI SSE chunks (`data: {…}\n\n`) into Ollama NDJSON (`{…}\n`).
- Preserves the `tool_calls` delta structure.
- Adds a final chunk with `done: true, done_reason: "stop"` (or `"tool_calls"` if there were tool calls).

### 9.4 Tool calls detection

`internal/balancer/llamacpp_toolcall_detector.go`:
- **`detectAndExtractToolCallsFromContent`** — main function, tries several formats.
- **`detectHermesToolCallsInContent`** — `<tool_call>{…}</tool_call>` (Qwen, Hermes).
- **`detectMistralToolCallsInContent`** — `[TOOL_CALLS][…]` (Mistral).
- **`detectLlamaPythonTagInContent`** — `<|python_tag|>{…}` (Llama-3).
- **`detectStandardArrayToolCalls`** — standard array.

If the model emits a tool_call in `content` while `tool_calls` is empty — we extract it from content and populate the OpenAI structure.

See [`runbook-tools.md`](runbook-tools.md) for details.

---

## 10. Typical scenarios

### 10.1 Setting a long context for gemma-4

```bash
# 1. Create the profile
curl -X PUT http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M \
  -H "Content-Type: application/json" \
  -d '{"contextLength": 32768, "batchSize": 1024, "numGpuLayers": -1, "flashAttn": true}'

# 2. Apply (reload on all backends)
curl -X POST http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M/apply

# 3. Request from Cline with a long system prompt
curl -X POST http://localhost:18081/api/chat \
  -H "Content-Type: application/json" \
  -d '{"model":"gemma-4","messages":[{"role":"system","content":"... long prompt ..."},{"role":"user","content":"hi"}]}'
```

### 10.2 Fixing HTTP 413 on a tools request

See [`runbook-tools.md` scenario A](runbook-tools.md).

### 10.3 Resetting a reload loop (HTTP 413 `ReloadLoopLimitError`)

```bash
# After fixing the cause (for example, freeing up VRAM):
curl -X POST http://localhost:18081/api/v1/nctx-reload/cppworker-gpu-1/reset

# If the counter is on the cppworker side — the R-6 endpoint is needed:
curl -X POST http://localhost:18081/api/v1/cppworker/reset-reload-counter
# (after R-6 is implemented)
```

### 10.4 Full .env.bundled configuration for long context

```env
# RAM fallback for n_ctx
CPPWORKER_RAM_FALLBACK_N_CTX=true
CPPWORKER_RAM_FALLBACK_MAX_N_CTX=128000
CPPWORKER_RAM_FALLBACK_GPU_LAYERS=0  # CPU-only fallback on OOM

# n_ctx auto-reload on the balancer
LB_NCTX_RELOAD_ENABLED=true
LB_NCTX_RELOAD_MAX_N_CTX=131072
LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR=0.85
LB_NCTX_RELOAD_TIMEOUT_SEC=120

# Write timeout for long streaming responses
CPPWORKER_WRITE_TIMEOUT=1800  # 30 minutes
```

---

## 11. Related documents

- [`api.md`](api.md) — full REST API specification.
- [`runbook-tools.md`](runbook-tools.md) — diagnostics for tools/tool_calls.
- [`audit-2026-06.md`](audit-2026-06.md) — implementation status.
- [`../.clinerules`](../.clinerules) §3–4 — working rules for n_ctx/RAM fallback.
- [`../plans/README.md`](../plans/README.md) — roadmap (R-1…R-7).
