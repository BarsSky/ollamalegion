# OllamaLegion API Reference

Complete REST API and WebSocket endpoints documentation.

## Table of Contents

1. [API Overview](#api-overview)
2. [Authentication](#authentication)
3. [REST API Endpoints](#rest-api-endpoints)
4. [WebSocket API](#websocket-api)
5. [Request Examples](#request-examples)
6. [OpenAPI Specification](#openapi-specification)

---

## API Overview

The load balancer provides a REST API for cluster management and metrics retrieval.

### Base URL

```
# Management API (cluster management, metrics, sessions)
http://localhost:18081

# HTTPS (if TLS is enabled)
https://localhost:8443
```

### CppWorker (llama.cpp) API

CppWorker exposes the following APIs in addition to the proxied Ollama endpoints:

| Endpoint | Method | Description | Compatibility |
|----------|--------|-------------|---------------|
| `/api/generate` | `POST` | Text generation (NDJSON) | Ollama-compatible |
| `/api/chat` | `POST` | Chat (NDJSON, streaming) | Ollama-compatible |
| `/api/embeddings` | `POST` | Embeddings | Ollama-compatible |
| `/api/tags` | `GET` | List models (GGUF) | Ollama-compatible |
| `/api/ollama/generate` | `POST` | Generation with options | Ollama-compatible |
| `/api/ollama/tags` | `GET` | List models | Ollama-compatible |
| `/v1/chat/completions` | `POST` | Chat completions (SSE) | **OpenAI-compatible** |
| `/v1/completions` | `POST` | Text completions (SSE) | **OpenAI-compatible** |
| `/v1/embeddings` | `POST` | Embeddings | **OpenAI-compatible** |
| `/v1/models` | `GET` | List models | **OpenAI-compatible** |
| `/health` | `GET` | Health check | |
| `/api/models/load` | `POST` | Load GGUF (protection against double-load) | |
| `/api/models/unload` | `POST` | Unload model | |
| `/api/models/reload` | `POST` | Unload + load with new parameters (n_ctx, batchSize, numGpuLayers) | |
| `/api/hf/*` | `GET/POST` | HuggingFace integration | |

#### CppWorker: Ollama-compatible fields for `/api/generate` and `/api/ollama/generate`

Full mapping of `options.*` to llama.cpp parameters (via `bridge.GenerationParams`):

| Request field | llama.cpp parameter | Notes |
|--------------|--------------------|-------|
| `options.temperature` | `Temperature` | |
| `options.top_p` | `TopP` | |
| `options.top_k` | `TopK` | |
| `options.min_p` | `MinP` | |
| `options.typical_p` | `TypicalP` | |
| `options.tfs_z` | `TfsZ` | |
| `options.num_predict` | `NPredict` | |
| `options.num_keep` | `NKeep` | |
| `options.repeat_penalty` | `RepeatPenalty` | |
| `options.frequency_penalty` | `FrequencyPenalty` | |
| `options.presence_penalty` | `PresencePenalty` | |
| `options.repeat_last_n` | `RepeatLastN` | |
| `options.mirostat` | `Mirostat` | |
| `options.mirostat_tau` | `MirostatTau` | |
| `options.mirostat_eta` | `MirostatEta` | |
| `options.seed` | `Seed` | `0` is a valid value |
| `options.num_ctx` | `NCtxOverride` | per-request n_ctx |
| `options.stop` | `Antiprompts` | string or []string |

Top-level aliases: `temperature`, `topP`, `topK`, `minP`, `typicalP`, `tfsZ`, `maxTokens`, `repeatPenalty`, `frequencyPenalty`, `presencePenalty`, `seed`, `numCtx`.

Additional fields: `system` (prepended to the prompt if `raw != true`), `template`, `raw`, `format` (accepted for compatibility), `keep_alive` (accepted, unload timer not implemented), `context` (accepted, KV-cache follow-up not implemented), `images` (not supported, accepted for compatibility).

#### CppWorker: `/api/generate` statistics

The response contains real metrics:

- `load_duration` — time from model load until generation starts (μs).
- `prompt_eval_count` — number of prompt tokens, counted via the model's tokenizer (`bridge_count_tokens` / `Backend.CountTokens`).
- `eval_count` — number of tokens in the response.
- `total_duration`, `prompt_eval_duration`, `eval_duration` — durations in μs.

If the model returns an empty `response: ""`, CppWorker returns HTTP 500 with an `error` field.

#### CppWorker: `/api/chat`

- Supports `messages` with `system`, `user`, `assistant` roles.
- Prompt construction uses `backend.ApplyChatTemplate` (chat template from GGUF); if no template is available, falls back to a naive format (`<|user|>` / `<start_of_turn>user`).
- Supports `stream`, `temperature`, `max_tokens`, `num_ctx`, `options.stop`.
- Returns `message.role="assistant"`, `done=true` in the final chunk.

#### CppWorker: protection against double-loading

`POST /api/models/load` checks `backend.GetModel(name)` before calling `LoadModelWithOpts`:

- If the model is already loaded with the same path and parameters (`ContextSize`, `BatchSize`, `GPULayers`, `FlashAttnType`, `NUMA`, `UseMmap`, `TensorSplit`) — returns `status: "already_loaded"` and does not touch VRAM.
- If the path or parameters differ — `UnloadModel` is called first, then `LoadModelWithOpts` (reload-in-place).
- Two concurrent loads of the same model are serialized via `IsModelLoading`; the second one receives a 503 with `loading: true`.

#### CppWorker: Tool Calling (Function Calling)

CppWorker supports function calling in OpenAI/Ollama-compatible formats:

- `/api/chat` (Ollama NDJSON) with a `tools: [...]` field in the request.
- `/v1/chat/completions` (OpenAI SSE) with a `tools: [...]` field in the request.

Behavior:

1. When `tools` is present in the request, cppworker extends the system prompt with descriptions of available functions (`buildToolsSystemPrompt` in `cmd/cppworker/tool_calls.go`).
2. After generation, cppworker attempts to parse `tool_calls` from the model's plain-text output via `parseToolCallsFromOutput`.
3. If recognized — returns `finish_reason: "tool_calls"` and `message.tool_calls`.
4. If NOT recognized — returns a normal text response with `finish_reason: "stop"`.

##### Supported tool_call formats from models

cppworker (`parseToolCallsFromOutput`) and the balancer (`detectAndExtractToolCallsFromContent`) recognize the following formats:

| # | Format | Example | Models |
|---|--------|---------|--------|
| 1 | Ollama/JSON array | `[{"id":"call_x","type":"function","function":{"name":"search","arguments":"..."}}]` | Gemma-4, llama.cpp chat template |
| 2 | Hermes / Qwen 2.5 (XML) | `<tool_call>{"name":"search","arguments":{...}}</tool_call>` | NousResearch Hermes, Qwen 2.5 |
| 3 | Llama-3.x python_tag | `<\|python_tag\|>{"name":"search","parameters":{...}}<\|eom_id\|>` | Llama-3.1+, Meta Llama-3 instruct |
| 4 | Mistral Nemo | `[TOOL_CALLS][{"name":"search","arguments":{...}}]` | Mistral Nemo, Mistral-7B-instruct |
| 5 | JSON inside markdown | ```` ```json [...] ``` ```` | Universal |
| 6 | Single-object JSON | `{"name":"search","arguments":{...}}` | Custom models |
| 7 | JSON with prefix | `Reasoning... [{"id":"call_x",...}]` | Models with chain-of-thought |

Recognition follows priority: first Hermes, then Llama-3, then Mistral, then standard JSON array, then single-object.

##### `tools` fields (OpenAI/Ollama)

```json
{
  "model": "my-model",
  "messages": [{"role": "user", "content": "What is the weather?"}],
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "search",
        "description": "Search the web",
        "parameters": {
          "type": "object",
          "properties": {
            "q": {"type": "string", "description": "Search query"}
          },
          "required": ["q"]
        }
      }
    }
  ]
}
```

`tool_choice` is also supported:
- `"auto"` (default) — the model decides whether to call a tool.
- `"none"` — the model must not call any tools.
- `{"type": "function", "function": {"name": "search"}}` — force a specific tool call.

##### Follow-up messages from tool results

OpenWebUI and other clients send the tool result as a message with `role: "tool"`:

```json
{
  "role": "tool",
  "tool_call_id": "call_search",
  "name": "search",
  "content": "Result of the search..."
}
```

cppworker serializes tool messages into the model prompt via `buildChatPrompt` (format `tool_call_result(<name>): <content>`).

##### Streaming with tools

For streaming (`stream: true`) when `tools` is present (Ollama format `/api/chat`):

- cppworker streams tokens as usual via `generateStreamWithRamFallback` (real-time feedback).
- In parallel, it accumulates the full output in a buffer.
- After generation completes, it checks for `tool_calls` via `parseToolCallsFromOutput`.
- If `tool_calls` are detected — it clears `content` and emits a final NDJSON chunk with `done: true`, `done_reason: "tool_calls"`, `message.tool_calls`.
- If NOT detected — it emits a normal chunk with `done_reason: "stop"`.

> **Note:** Previously, when `stream=true && tools!=[]`, cppworker returned non-streaming JSON via `writeJSON`, which broke OpenWebUI (it expected NDJSON chunks and interpreted the response as "source used" with no visible answer). This is now fixed via `writeChatStreamResponseWithTools` in `cmd/cppworker/handlers_chat.go`.

For OpenAI-format `/v1/chat/completions` streaming with tools, `writeToolCallsStream` or `writeStaticTextStream` (SSE) is used.

The balancer (`internal/balancer/llamacpp_translate_resp.go`) additionally tries to parse `tool_calls` from SSE chunk content via `detectAndExtractToolCallsFromContent` to recover structured tool_calls even when cppworker did not recognize them.

##### Known limitations

- Ollama Modelfile templates (`template`, `system` in `.gguf.json`) are not applied — cppworker uses the chat template from GGUF or a naive fallback.
- Ollama registry (`ollama pull llama3`) is not supported; to load models use `hf:<repo>/<file.gguf>` via `/api/pull` or `/api/hf/download`.
- Vision/multimodal tools (image inputs) are not supported — the model receives only the text part.

#### CppWorker: per-model profiles (n_ctx management)

**Full documentation:** [cppworker-model-params.md](../cppworker-model-params.md).

These endpoints let you set `n_ctx` and other inference parameters for a specific model on cppworker. Used to solve the `n_ctx overflow` problem in Cline/OpenWebUI.

| Endpoint | Method | Description | Auth |
|----------|--------|-------------|------|
| `/api/v1/cppworker/model-profiles` | `GET` | List all per-model profiles | Yes |
| `/api/v1/cppworker/model-profiles/{name}` | `GET` | Get profile for a model | Yes |
| `/api/v1/cppworker/model-profiles/{name}` | `PUT` | Create/update profile + save to config.json | Yes |
| `/api/v1/cppworker/model-profiles/{name}` | `DELETE` | Delete profile | Yes |
| `/api/v1/cppworker/model-profiles/{name}/apply` | `POST` | Save + reload model on all llama_cpp backends | Yes |

**3-tier resolver (priority when forwarding n_ctx to cppworker):**

1. `body.options.num_ctx` (Ollama) / `body.num_ctx` (OpenAI) — **always wins** (Tier 1)
2. `config.LlamaCppModelProfiles[modelName].ContextLength` — Tier 2
3. `state.Backend.CppWorkerConfig.ContextLength` (per-backend default) — Tier 3

The resolved value is forwarded to cppworker via the HTTP header `X-Cpp-Ctx: <value>`.

**Example (create a profile with n_ctx=32768 for gemma-4):**

```bash
curl -X PUT http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M \
  -H "Content-Type: application/json" \
  -H "X-API-Token: your-token" \
  -d '{
    "contextLength": 32768,
    "batchSize": 1024,
    "numGpuLayers": -1,
    "flashAttn": true,
    "notes": "Cline + long system prompt"
  }'
```

**Example (apply — save + reload on all backends):**

```bash
curl -X POST http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M/apply \
  -H "Content-Type: application/json" \
  -H "X-API-Token: your-token" \
  -d '{ "contextLength": 65536 }'
```

**Profile validation:** `contextLength` ∈ `[256, 262144]` (256K is the native max for gemma-4), `batchSize >= 1`, `numGpuLayers >= -1` (`-1` = all layers).

### GGUF Backend Proxy API (via the balancer)

The balancer provides a **universal proxy** for all CppWorker endpoints, so WebUI and external clients can talk to CppWorker **through the balancer** instead of making direct HTTP requests (which break in Docker environments due to CORS and the inaccessibility of `host.docker.internal` from a browser).

**URL format:**

```
/api/v1/gguf/backends/{backendId}/proxy/<cppworker-path>
```

**Examples:**

| Request | Backend `llama_gpu` | CppWorker |
|---|---|---|
| `GET /api/v1/gguf/backends/llama_gpu/proxy/info` | llama_gpu | `GET /info` |
| `GET /api/v1/gguf/backends/llama_gpu/proxy/api/gpu` | llama_gpu | `GET /api/gpu` |
| `GET /api/v1/gguf/backends/llama_gpu/proxy/api/models` | llama_gpu | `GET /api/models` |
| `GET /api/v1/gguf/backends/llama_gpu/proxy/api/models/files` | llama_gpu | `GET /api/models/files` |
| `GET /api/v1/gguf/backends/llama_gpu/proxy/api/hf/search?query=...` | llama_gpu | `GET /api/hf/search?query=...` |
| `GET /api/v1/gguf/backends/llama_gpu/proxy/api/hf/files?modelId=...` | llama_gpu | `GET /api/hf/files?modelId=...` |
| `POST /api/v1/gguf/backends/llama_gpu/proxy/api/hf/download` | llama_gpu | `POST /api/hf/download` |
| `GET /api/v1/gguf/backends/llama_gpu/proxy/api/hf/progress?modelId=...&filename=...` | llama_gpu | `GET /api/hf/progress?modelId=...&filename=...` |
| `GET /api/v1/gguf/backends/llama_gpu/proxy/api/hf/downloads` | llama_gpu | `GET /api/hf/downloads` |
| `POST /api/v1/gguf/backends/llama_gpu/proxy/api/hf/cancel` | llama_gpu | `POST /api/hf/cancel` |
| `POST /api/v1/gguf/backends/llama_gpu/proxy/api/models/load` | llama_gpu | `POST /api/models/load` |
| `POST /api/v1/gguf/backends/llama_gpu/proxy/api/models/unload` | llama_gpu | `POST /api/models/unload` |
| `POST /api/v1/gguf/backends/llama_gpu/proxy/api/models/delete` | llama_gpu | `POST /api/models/delete` |

#### Features

- **Transparent proxying**: HTTP method, headers (including `X-HF-Token`), query string and JSON body are forwarded to CppWorker unchanged.
- **Automatic host resolution**: if a backend is registered with `host=host.docker.internal`, the proxy replaces it with `localhost` (since `host.docker.internal` does not resolve in the browser).
- **Timeout**: 90 seconds (HF search/download can take up to 60-90s, especially search with parallel file fetching).
- **Clear error messages**: on network issues, `502 Bad Gateway` / `504 Gateway Timeout` is returned with the CppWorker address.

#### cURL examples

```bash
# Get the CppWorker version
curl http://localhost:18081/api/v1/gguf/backends/llama_gpu/proxy/info

# Download a model from HuggingFace (with HF token for gated repositories)
curl -X POST http://localhost:18081/api/v1/gguf/backends/llama_gpu/proxy/api/hf/download \
  -H 'Content-Type: application/json' \
  -H 'X-HF-Token: hf_xxxxxxxxxxxx' \
  -d '{"modelId":"TheBloke/Llama-2-7B-GGUF","filename":"llama-2-7b.Q4_K_M.gguf","revision":"main"}'

# Get the list of active downloads
curl http://localhost:18081/api/v1/gguf/backends/llama_gpu/proxy/api/hf/downloads

# Delete a model
curl -X POST http://localhost:18081/api/v1/gguf/backends/llama_gpu/proxy/api/models/delete \
  -H 'Content-Type: application/json' \
  -d '{"name":"llama-3-8b-q4_K_M.gguf"}'
```

#### Response codes

| Code | Reason |
|---|---|
| 200 / 202 | Successful response from CppWorker (status and body are copied verbatim) |
| 400 | Invalid URL (no `proxy/` section specified) |
| 404 | Backend with the specified ID not found, or its type is not `llama_cpp` |
| 502 | CppWorker unavailable (connection refused, host unresolvable) |
| 504 | CppWorker not responding (timeout) |
| Method != GET/POST/DELETE/PUT | 405 Method Not Allowed |

> **Streaming formats:**
> - `/api/generate`, `/api/chat`, `/api/ollama/generate` → **NDJSON** (`application/x-ndjson`)
> - `/v1/chat/completions`, `/v1/completions` → **SSE** (`text/event-stream`) with a final `[DONE]`

### Ollama API Proxy

The balancer proxies standard Ollama API endpoints on port `18080`. All Ollama requests pass through the balancer with session stickiness, model affinity, queue management, and retry/failover.

**Base URL:**
```
http://localhost:18080
```

| Endpoint | Method | Description | Proxy Features |
|----------|--------|-------------|----------------|
| `/api/generate` | `POST` | Text generation | Session stickiness, streaming/SSE, up to 3 retries |
| `/api/chat` | `POST` | Chat | Session stickiness, streaming/SSE, up to 3 retries |
| `/api/embed` | `POST` | Embeddings | Session stickiness, up to 3 retries |
| `/api/embeddings` | `POST` | Embeddings (legacy) | Session stickiness, up to 3 retries |
| `/api/tags` | `GET` | List models | **Aggregated** from all backends, deduplicated |
| `/api/version` | `GET` | Ollama version | Returns the balancer version |
| `/api/ps` | `GET` | Status of loaded models | **Aggregated** from all backends |
| `/api/show` | `POST` | Model information | Routed to the backend with the loaded model |
| `/api/create` | `POST` | Create model | Routed to the backend with the most free resources |
| `/api/pull` | `POST` | Pull model | Routed to the backend with the most free resources |
| `/api/delete` | `DELETE` | Delete model | **Broadcast** to all backends that have the model |
| `/api/copy` | `POST` | Copy model | Routed to the backend with the source model |
| `/api/push` | `POST` | Push model | Routed to the backend with the model |

**Headers for session management:**
- `X-Client-ID` — preferred stable client identifier (recommended)
- `X-Session-ID` — alternative session identifier
- `X-Client-Name` — client name (Cline, OpenWebUI, etc.) for monitoring

> **Note:** `X-Client-ID` without an ephemeral port guarantees session stability behind NAT. If no headers are provided, the IP without a port is used.

#### Detailed description of Ollama endpoints

##### GET /api/tags

Aggregates the list of local models from all available backends (including `healthy` and `degraded`). Models are deduplicated by the `name` field — if the same model is present on several backends, it appears only once in the final list. The result is sorted by name for deterministic order.

**Proxying features:**
- Parallel requests to all backends (5-second timeout each)
- Deduplication by the `name` field
- `healthy` and `degraded` backends are included

**Example response:**

```json
{
  "models": [
    {
      "name": "llama3.1:8b",
      "model": "llama3.1:8b",
      "modified_at": "2024-06-15T10:30:00Z",
      "size": 4928300000,
      "digest": "sha256:abc123...",
      "details": {
        "family": "llama",
        "parameter_size": "8B",
        "quantization_level": "Q4_0"
      }
    },
    {
      "name": "qwen2.5:14b",
      "model": "qwen2.5:14b",
      "modified_at": "2024-06-15T11:00:00Z",
      "size": 8965234567,
      "digest": "sha256:def456...",
      "details": {
        "family": "qwen",
        "parameter_size": "14B",
        "quantization_level": "Q4_K_M"
      }
    }
  ]
}
```

##### GET /api/ps

Aggregates the list of running models from all backends. Shows models currently loaded in VRAM.

**Example response:**

```json
{
  "models": [
    {
      "name": "llama3.1:8b",
      "size": 4928300000,
      "digest": "sha256:abc123...",
      "expires_at": "2024-06-15T11:00:00Z",
      "size_vram": 6000000000
    }
  ]
}
```

##### GET /api/version

Returns the balancer version plus the versions of all available backends.

**Example response:**

```json
{
  "version": "ollamalegion-1.0.0",
  "ollamaVersions": {
    "backend-1": "0.3.0",
    "backend-2": "0.3.0"
  }
}
```

##### POST /api/show

Model information. The request is routed to the backend where the model is already loaded (RunningModels). If the model is not found — falls back to any healthy backend.

**Request:**

```json
{
  "name": "llama3.1:8b"
}
```

**Proxying features:**
- Routing via `findBackendWithModel()` → `RunningModels`
- Fallback to `selectAnyHealthy()` if the model is not loaded

##### POST /api/create

Create a model. The request is routed to the backend with the most free resources.

**Request:**

```json
{
  "name": "custom-model",
  "modelfile": "FROM llama3.1:8b\nPARAMETER temperature 0.7"
}
```

**Proxying features:**
- Routing via `selectBackendByResources()`
- SSE streaming response with creation progress

##### POST /api/pull

Pull a model. The request is routed to the backend with the most free resources.

**Request:**

```json
{
  "name": "llama3.1:8b"
}
```

**Proxying features:**
- Routing via `selectBackendByResources()`
- SSE streaming response with pull progress (status + completed/total)

##### DELETE /api/delete

Delete a model from all backends where it is present.

**Request:**

```json
{
  "name": "llama3.1:8b"
}
```

**Proxying features:**
- **Broadcast** to all backends that have the model (`findBackendsWithModel`)
- Parallel requests, the first successful response is returned
- Bodies of unused responses are closed (protection against connection leaks)

##### POST /api/copy

Copy a model. The request is routed to the backend that has the source model.

**Request:**

```json
{
  "source": "llama3.1:8b",
  "destination": "llama3.1:8b-custom"
}
```

**Proxying features:**
- Routing via `findBackendWithModel(req.Source)`
- Body is restored for proxying (`readBody` + `io.NopCloser`)

##### POST /api/push

Push a model. The request is routed to the backend that has the model.

**Request:**

```json
{
  "name": "llama3.1:8b"
}
```

**Proxying features:**
- Routing via `findBackendWithModel()`
- SSE streaming response with push progress

#### Data format

- **Request**: JSON
- **Response**: JSON
- **Content-Type**: `application/json`

#### Response codes

| Code | Description |
|------|-------------|
| `200` | Successful request |
| `201` | Resource created |
| `400` | Bad request |
| `401` | Unauthorized |
| `403` | Access denied |
| `404` | Resource not found |
| `409` | Conflict (resource already exists) |
| `500` | Internal server error |
| `503` | Service unavailable |

---

## Authentication

The API uses token-based authentication via the `X-API-Token` header. Authentication can be **enabled or disabled** in the balancer configuration (`config.json` → `auth.enabled`).

| Auth state | Behavior |
|------------|----------|
| `enabled: false` | All endpoints are public, token not required |
| `enabled: true` | Protected endpoints require a valid token in the header |

### Enabling/disabling authentication

In `config.json`:

```json
{
  "auth": {
    "enabled": true,
    "tokens": ["your-master-token"],
    "headerName": "X-API-Token"
  }
}
```

- `enabled: false` — public access to all endpoints (convenient for development and internal networks)
- `tokens` — list of valid tokens; the **first token** is the master token (used to generate new tokens)
- `headerName` — HTTP header name (default `X-API-Token`)

### Endpoint / authentication matrix

| Endpoint | Method | Auth required | Notes |
|----------|--------|---------------|-------|
| `GET /api/v1/health` | — | No | Public, used by WebUI to detect settings |
| `GET /api/v1/ratelimit/status` | — | No | Public |
| `GET /api/v1/auth/status` | — | Yes | Requires token |
| `POST /api/v1/auth/token` | — | Master only | Generate a new token |
| `GET /api/v1/cluster` | — | Yes | |
| `GET /api/v1/backends` | — | Yes | |
| `POST /api/v1/backends` | — | Yes | |
| `GET /api/v1/metrics` | — | Yes | |
| `GET /api/v1/models` | — | Yes | |
| `GET /api/v1/sessions` | — | Yes | |
| `GET /api/v1/agents/stats` | — | Yes | |
| `GET /api/v1/queue/stats` | — | Yes | Request queue statistics |
| `WebSocket /ws/metrics` | — | Yes (if auth enabled) | Token passed via query parameter |

### Passing the token over HTTP

```bash
# cURL
curl -X GET http://localhost:18081/api/v1/cluster \
  -H "X-API-Token: your-api-token"

# Python requests
import requests
headers = {"X-API-Token": "your-api-token"}
response = requests.get("http://localhost:18081/api/v1/cluster", headers=headers)

# JavaScript fetch
fetch('http://localhost:18081/api/v1/cluster', {
  headers: {'X-API-Token': 'your-api-token'}
})
```

### Passing the token in WebSocket

Browsers do **not** support setting arbitrary HTTP headers when creating a WebSocket. Therefore the token is passed via a **query parameter**:

```javascript
// Correct way
const ws = new WebSocket('ws://localhost:18081/ws/metrics?token=your-api-token');

// Wrong — headers are ignored by the browser
const ws = new WebSocket('ws://...'); // new WebSocket(url, protocols) does not support headers
```

On the server side (`wsMetricsHandler`):
- If `auth.enabled: false` → WebSocket upgrade is performed without token verification
- If `auth.enabled: true` → a non-empty `?token=...` is required; the token is validated via `Authenticate()`

### Token Management

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/auth/status` | GET | Authentication status |
| `/api/v1/auth/token` | POST | Generate new token (requires master token) |
| `/api/v1/auth/revoke` | POST | Revoke a token (requires master token) |

---

## REST API Endpoints

### Health

#### GET /api/v1/health

API health check. **Does not require authentication** — used by WebUI and external health checks to determine system state.

The response includes metadata that clients need to connect correctly:

| Field | Type | Description |
|-------|------|-------------|
| `status` | string | API state: `"healthy"` |
| `timestamp` | string | Server time in ISO 8601 |
| `version` | string | API version |
| `authEnabled` | boolean | `true` — authentication enabled, `false` — disabled |
| `authHeader` | string | Header name for the token (default `"X-API-Token"`) |
| `wsEndpoint` | string | WebSocket endpoint path (default `"/ws/metrics"`) |

**Example response when auth is disabled:**

```json
{
  "status": "healthy",
  "timestamp": "2024-01-15T10:30:00Z",
  "version": "1.0.0",
  "authEnabled": false,
  "authHeader": "X-API-Token",
  "wsEndpoint": "/ws/metrics"
}
```

**Example response when auth is enabled:**

```json
{
  "status": "healthy",
  "timestamp": "2024-01-15T10:30:00Z",
  "version": "1.0.0",
  "authEnabled": true,
  "authHeader": "X-API-Token",
  "wsEndpoint": "/ws/metrics"
}
```

> **How WebUI works:** When the dashboard loads, it first calls `GET /api/v1/health` **without a token**. If `authEnabled: false` — the application starts immediately. If `authEnabled: true` — a modal window is shown for token entry.

---

### Cluster

#### GET /api/v1/cluster

Get cluster state information.

**Response:**

```json
{
  "timestamp": "2024-01-15T10:30:00Z",
  "totalBackends": 2,
  "healthyBackends": 2,
  "totalRequests": 1500,
  "activeRequests": 5,
  "queuedRequests": 0,
  "backends": [
    {
      "id": "gpu-1",
      "timestamp": "2024-01-15T10:30:00Z",
      "status": "healthy",
      "gpu": {
        "usagePercent": 45.5,
        "memoryTotal": 24576,
        "memoryUsed": 12000,
        "temperature": 65,
        "powerUsage": 250,
        "powerLimit": 450
      },
      "system": {
        "cpuUsagePercent": 30.2,
        "memoryTotal": 65536,
        "memoryUsed": 20000
      },
      "ollama": {
        "runningModels": [
          {"name": "llama3.1:70b", "vramUsage": 18000}
        ],
        "activeRequests": 3,
        "requestsPerSecond": 12.5
      }
    }
  ]
}
```

---

### Metrics

#### GET /api/v1/metrics

Get metrics for all backends in the cluster.

**Response:**

```json
{
  "timestamp": "2024-01-15T10:30:00Z",
  "totalBackends": 2,
  "healthyBackends": 2,
  "backends": [
    {
      "id": "gpu-1",
      "timestamp": "2024-01-15T10:30:00Z",
      "gpu": {...},
      "system": {...},
      "ollama": {...}
    }
  ]
}
```

#### GET /api/v1/metrics/{backend_id}

Get metrics for a specific backend.

**Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `backend_id` | path | Backend ID |

**Response:**

```json
{
  "id": "gpu-1",
  "timestamp": "2024-01-15T10:30:00Z",
  "gpu": {
    "usagePercent": 45.5,
    "memoryTotal": 24576,
    "memoryUsed": 12000,
    "memoryFree": 12576,
    "temperature": 65,
    "powerUsage": 250,
    "powerLimit": 450,
    "gpuClock": 1800,
    "memClock": 10000
  },
  "system": {
    "cpuUsagePercent": 30.2,
    "memoryTotal": 65536,
    "memoryUsed": 20000,
    "memoryFree": 45536,
    "diskTotal": 1000000,
    "diskUsed": 500000,
    "diskFree": 500000,
    "networkRX": 1048576,
    "networkTX": 524288
  },
  "ollama": {
    "runningModels": [
      {
        "name": "llama3.1:70b",
        "size": 70000000000,
        "vramUsage": 18000,
        "expiresAt": "2024-01-15T11:00:00Z",
        "digest": "sha256:..."
      }
    ],
    "activeRequests": 3,
    "totalRequests": 1500,
    "avgResponseTime": 250.5,
    "requestsPerSecond": 12.5
  }
}
```

---

### Backends

#### GET /api/v1/backends

Get the list of all backends.

**Response:**

```json
{
  "backends": [
    {
      "id": "gpu-1",
      "name": "GPU Server 1",
      "host": "192.168.13.66",
      "ollamaPort": 11434,
      "agentPort": 18032,
      "weight": 1,
      "maxConcurrentRequests": 10,
      "labels": ["nvidia", "rtx4090"],
      "status": "healthy",
      "lastHealthCheck": "2024-01-15T10:30:00Z",
      "consecutiveFailures": 0,
      "activeRequests": 3
    }
  ],
  "total": 2
}
```

#### POST /api/v1/backends

Add a new backend.

**Request body:**

```json
{
  "id": "gpu-3",
  "name": "GPU Server 3",
  "host": "192.168.13.80",
  "ollamaPort": 11434,
  "agentPort": 18032,
  "weight": 1,
  "maxConcurrentRequests": 10,
  "labels": ["nvidia", "a100"]
}
```

**Response:**

```json
{
  "success": true,
  "backend": {...},
  "message": "Backend added successfully. Health check will run automatically."
}
```

#### GET /api/v1/backends/{backend_id}

Get information about a backend.

**Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `backend_id` | path | Backend ID |

#### PUT /api/v1/backends/{backend_id}

Update backend parameters.

**Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `backend_id` | path | Backend ID |

**Request body:**

```json
{
  "name": "GPU Server 1 (Updated)",
  "weight": 2,
  "maxConcurrentRequests": 20,
  "labels": ["nvidia", "rtx4090"]
}
```

**Response:**

```json
{
  "success": true,
  "backend": {...},
  "message": "Backend updated successfully"
}
```

#### POST /api/v1/backends/{backend_id}/reconfigure

Reconfigure a backend with new environment variables (envVars). Requires `force: true` for confirmation.

**Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `backend_id` | path | Backend ID |

**Request body:**

```json
{
  "envVars": {
    "OLLAMA_NUM_PARALLEL": "4",
    "OLLAMA_MAX_LOADED_MODELS": "2"
  },
  "force": true
}
```

**Response:**

```json
{
  "success": true,
  "message": "Reconfiguration initiated"
}
```

#### GET /api/v1/backends/{backend_id}/launch-config

Get the Ollama launch configuration for a backend (runtime flags, env vars).

**Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `backend_id` | path | Backend ID |

**Response:**

```json
{
  "backend_id": "gpu-1",
  "launch_config": {
    "numGpuLayers": -1,
    "contextLength": 4096,
    "numParallel": 4,
    "numThreads": 8,
    "batchSize": 512,
    "cpuOnly": false,
    "flashAttention": false,
    "kvSize": 512,
    "tensorSplit": null,
    "mainGpu": 0
  },
  "env_vars": {
    "OLLAMA_NUM_PARALLEL": "4",
    "OLLAMA_MAX_LOADED_MODELS": "2"
  }
}
```

#### GET /api/v1/backends/{backend_id}/models

Get the list of models on a specific backend (RunningModels).

**Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `backend_id` | path | Backend ID |

**Response:**

```json
{
  "backend_id": "gpu-1",
  "models": [
    {
      "name": "llama3.1:8b",
      "digest": "sha256:abc123...",
      "size": 4928300000,
      "vram_usage": 6000000000,
      "expires_at": "2024-06-15T11:00:00Z"
    }
  ]
}
```

#### DELETE /api/v1/backends/{backend_id}

Remove a backend.

**Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `backend_id` | path | Backend ID |

**Response:**

```json
{
  "success": true,
  "id": "gpu-1",
  "message": "Backend removed successfully"
}
```

---

### Sessions

#### GET /api/v1/sessions

Get the list of active sessions.

**Response:**

```json
{
  "sessions": [],
  "total": 0
}
```

#### DELETE /api/v1/sessions

Clear all sessions.

**Response:**

```json
{
  "success": true,
  "message": "All sessions cleared"
}
```

#### DELETE /api/v1/sessions/{session_id}

Delete a specific session.

**Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `session_id` | path | Session ID |

---

### Models

#### GET /api/v1/models

Get the list of running models on all backends.

**Response:**

```json
{
  "models": {
    "gpu-1": ["llama3.1:70b", "mistral:7b"],
    "gpu-2": ["llama3.1:8b"]
  }
}
```

#### GET /api/v1/models/operations

Get the status of active model operations (pull, create, delete).

**Response:**

```json
{
  "operations": [
    {
      "id": "pull-llama3.1-8b",
      "type": "pull",
      "model": "llama3.1:8b",
      "backend_id": "gpu-1",
      "status": "in_progress",
      "progress": 65,
      "started_at": "2024-01-15T10:30:00Z"
    }
  ],
  "total_active": 1
}
```

---

### Proxy Logs

#### GET /api/v1/proxy/logs

Get the logs of proxied requests (HTTP access log).

**Response:**

```json
{
  "logs": [
    {
      "timestamp": "2024-01-15T10:30:00Z",
      "method": "POST",
      "path": "/api/generate",
      "model": "llama3.1:8b",
      "backend_id": "gpu-1",
      "status_code": 200,
      "duration_ms": 250,
      "client_ip": "192.168.1.100"
    }
  ],
  "total": 1500
}
```

---

### Candidates

#### GET /api/v1/candidates

Get groups of candidate backends by priority for all models. Used in the monitor to display the "Candidate Backends" section.

**Response:**

```json
{
  "models": {
    "llama3.1:8b": {
      "P1_loaded": ["gpu-1", "gpu-2"],
      "P2_warming": [],
      "P3_free": ["gpu-3"],
      "P4_fallback": ["gpu-4"]
    }
  }
}
```

| Priority | Description |
|----------|-------------|
| `P1_loaded` | Backends with the model already loaded |
| `P2_warming` | Backends where the model is being loaded |
| `P3_free` | Backends with free resources (can load) |
| `P4_fallback` | All healthy backends (resource-based scoring) |

---

### Agents

#### GET /api/v1/agents/stats

Get statistics for all registered agents.

**Response:**

```json
{
  "totalAgents": 2,
  "healthyAgents": 2,
  "agents": [
    {
      "id": "gpu-1",
      "hostname": "gpu-server-1",
      "status": "healthy",
      "lastHeartbeat": "2024-01-15T10:30:00Z"
    }
  ]
}
```

#### GET /api/v1/agents/{agent_id}

Get information about a specific agent.

**Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `agent_id` | path | Agent ID |

#### Agents (Registration & Metrics)

#### POST /api/v1/agents/register

Register an agent in the balancing system. The agent sends this request at startup to auto-register with the cluster.

**Authentication:** Required (API Token)

**Request body:**
```json
{
  "agentId": "gpu-1",
  "hostname": "gpu-server-1",
  "host": "192.168.1.100",
  "ollamaPort": 11434,
  "agentPort": 18032,
  "gpuCount": 1,
  "name": "GPU Server 1",
  "labels": ["nvidia", "rtx4090"]
}
```

**Request parameters:**

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `agentId` | string | Yes | Unique agent identifier |
| `hostname` | string | No | Agent hostname |
| `host` | string | No | Agent IP address (if not specified, hostname or RemoteAddr is used) |
| `ollamaPort` | int | No | Ollama API port (default: 11434) |
| `agentPort` | int | No | Agent port (default: 18032) |
| `gpuCount` | int | No | Number of GPUs |
| `name` | string | No | Display name of the agent |
| `labels` | string[] | No | Labels for agent classification |

**Response:** 201 Created (new agent) or 200 OK (existing update)
```json
{
  "success": true,
  "action": "created",
  "agentId": "gpu-1",
  "backend": {
    "id": "gpu-1",
    "name": "GPU Server 1",
    "host": "192.168.1.100",
    "ollamaPort": 11434,
    "agentPort": 18032,
    "weight": 1,
    "maxConcurrentRequests": 10,
    "labels": ["nvidia", "rtx4090"],
    "status": "starting"
  },
  "message": "Agent registered successfully. Backend added to the pool."
}
```

**Example request (cURL):**
```bash
curl -X POST http://localhost:18081/api/v1/agents/register \
  -H "Content-Type: application/json" \
  -H "X-API-Token: your-api-token" \
  -d '{
    "agentId": "gpu-1",
    "host": "192.168.1.100",
    "ollamaPort": 11434,
    "agentPort": 18032,
    "name": "GPU Server 1",
    "labels": ["nvidia", "rtx4090"]
  }'
```

---

#### POST /api/v1/agents/metrics

Send metrics from the agent to the balancer. The agent must periodically send metrics to monitor state.

**Authentication:** Required (API Token)

**Headers:**

| Header | Value | Description |
|--------|-------|-------------|
| `X-Agent-ID` | string | Agent ID (required) |

**Request body — full agent metrics structure:**

> **Important:** The `activeRequests`, `totalRequests`, `avgResponseTime`, `requestsPerSecond` fields in the `ollama` section are returned by the agent as `0` and are **overridden by the balancer** based on its internal proxy counter.

```json
{
  "id": "gpu-1",
  "timestamp": "2024-01-15T10:30:00Z",
  "gpu": {
    "usagePercent": 45.5,
    "memoryTotal": 24576,
    "memoryUsed": 12000,
    "memoryFree": 12576,
    "temperature": 65,
    "powerUsage": 250,
    "powerLimit": 450,
    "gpuClock": 1800,
    "memClock": 10000
  },
  "system": {
    "cpuUsagePercent": 30.2,
    "memoryTotal": 65536,
    "memoryUsed": 20000,
    "memoryFree": 45536,
    "diskTotal": 1000000,
    "diskUsed": 500000,
    "diskFree": 500000,
    "networkRX": 1048576,
    "networkTX": 524288
  },
  "ollama": {
    "runningModels": [
      {
        "name": "llama3.1:70b",
        "size": 70000000000,
        "vramUsage": 18000,
        "expiresAt": "2024-01-15T11:00:00Z",
        "digest": "sha256:...",
        "family": "llama",
        "parameterSize": "70B",
        "quantization": "Q4_0"
      }
    ],
    "availableModels": [
      {
        "name": "mistral:7b",
        "size": 4000000000,
        "family": "mistral",
        "parameterSize": "7B",
        "quantization": "Q4_0"
      }
    ],
    "activeRequests": 0,
    "totalRequests": 0,
    "avgResponseTime": 0,
    "requestsPerSecond": 0,
    "maxModels": -1,
    "maxConcurrentRequests": -1,
    "freeSlots": 0
  }
}
```

**Response:** 200 OK
```json
{
  "status": "received"
}
```

**Example request (cURL):**
```bash
curl -X POST http://localhost:18081/api/v1/agents/metrics \
  -H "Content-Type: application/json" \
  -H "X-API-Token: your-api-token" \
  -H "X-Agent-ID: gpu-1" \
  -d '{
    "id": "gpu-1",
    "timestamp": "2024-01-15T10:30:00Z",
    "gpu": {
      "usagePercent": 45.5,
      "memoryTotal": 24576,
      "memoryUsed": 12000
    },
    "system": {...},
    "ollama": {...}
  }'
```

---

#### POST /api/v1/agents/heartbeat

Send a heartbeat from the agent to confirm liveness.

**Authentication:** Required (API Token)

**Headers:**

| Header | Value | Description |
|--------|-------|-------------|
| `X-Agent-ID` | string | Agent ID (required) |

**Request body:** Not required (may be empty)

**Response:** 200 OK
```json
{
  "status": "ok"
}
```

**Example request (cURL):**
```bash
curl -X POST http://localhost:18081/api/v1/agents/heartbeat \
  -H "X-API-Token: your-api-token" \
  -H "X-Agent-ID: gpu-1"
```

---

### Predictions

The balancer automatically computes predictions of critical states for each backend based on the history of metrics (up to 120 samples, 2-minute window).

#### GET /api/v1/predictions

Get predictions for all backends.

**Response:**
```json
{
  "gpu-1": {
    "backendId": "gpu-1",
    "secondsToCritical": 300,
    "criticalReason": "vram",
    "gpuUsageTrend": 0.5,
    "vramUsageTrend": 2.1,
    "ramUsageTrend": 0.3,
    "freeSlotsTrend": -0.8,
    "requestCapacity": 65.5
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `secondsToCritical` | float | Seconds until a critical state. `-1` = no threat |
| `criticalReason` | string | Reason: `gpu_usage`, `vram`, `ram`, `concurrent_requests`, `models_capacity`, `none` |
| `gpuUsageTrend` | float | GPU load trend (%/min) |
| `vramUsageTrend` | float | VRAM usage trend (%/min) |
| `ramUsageTrend` | float | RAM usage trend (%/min) |
| `freeSlotsTrend` | float | Free slots trend (slots/min) |
| `requestCapacity` | float | Current backend load (0-100%) |

---

### Auth

#### GET /api/v1/auth/status

Get the authentication system status.

**Response:**

```json
{
  "enabled": true,
  "headerName": "X-API-Token",
  "tokenCount": 3
}
```

#### POST /api/v1/auth/token

Create a new API token. Requires the master token.

**Response:**

```json
{
  "success": true,
  "token": "newly-generated-token-here",
  "message": "Token generated successfully"
}
```

#### DELETE /api/v1/auth/token

Revoke (delete) an API token. Requires the master token.

**Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `token` | query/body | Token to revoke |

**Response:**

```json
{
  "success": true,
  "message": "Token revoked successfully"
}
```

---

### Queue

#### GET /api/v1/queue/stats

Request queue statistics and balancer dispatch metrics.

**Response:**

```json
{
  "current_size": 0,
  "max_size": 100,
  "processed_total": 15420,
  "avg_wait_time_ms": 12,
  "workers": 4,
  "timeout_sec": 30,
  "dispatch_by_affinity": 8750,
  "dispatch_by_load": 4520,
  "dispatch_by_config": 2150
}
```

| Field | Type | Description |
|-------|------|-------------|
| `current_size` | int | Current number of requests in the queue |
| `max_size` | int | Maximum queue size |
| `processed_total` | int64 | Total requests processed |
| `avg_wait_time_ms` | int64 | Average wait time in the queue (ms) |
| `workers` | int | Number of workers |
| `timeout_sec` | int | Backend wait timeout |
| `dispatch_by_affinity` | int64 | Requests dispatched via **Model Affinity** (model already loaded on the backend) |
| `dispatch_by_load` | int64 | Requests dispatched via **Resource-Aware** selection (free resources) |
| `dispatch_by_config` | int64 | Requests dispatched by **weights/configuration** (fallback) |

> **How dispatch works:**
> 1. **Model Affinity** — the request goes to a backend where the model is already loaded (stages 1–3 in `selectBackend`).
> 2. **Resource-Aware** — selection by free resources (GPU, VRAM, CPU) with a prediction bonus.
> 3. **Config/Weight** — fallback selection by backend weights (when `UseEnhancedScoring=false`).

---

### Replication (Model Replication Groups API)

Manage model replication groups (Variant A). Allows configuring automatic scaling of model replicas across backends.

#### GET /api/v1/replication/groups

Get the list of all replication groups.

**Authentication:** Required (API Token)

**Response:**
```json
{
  "groups": [
    {
      "modelName": "llama3.1:70b",
      "minReplicas": 2,
      "maxReplicas": 4,
      "targetBackends": ["gpu-1", "gpu-2"]
    }
  ]
}
```

#### POST /api/v1/replication/groups

Create a new replication group.

**Request body:**
```json
{
  "modelName": "llama3.1:70b",
  "minReplicas": 2,
  "maxReplicas": 4,
  "targetBackends": ["gpu-1", "gpu-2"]
}
```

**Response:** 201 Created
```json
{
  "message": "group created",
  "group": { "...config..." }
}
```

#### GET /api/v1/replication/groups/{modelName}

Get detailed information about the replication group for the specified model.

**Response:**
```json
{
  "config": { "...group config..." },
  "states": [ "...instance states..." ],
  "stats": { "...group statistics..." }
}
```

#### PUT /api/v1/replication/groups/{modelName}

Update the configuration of a replication group.

#### DELETE /api/v1/replication/groups/{modelName}

Delete a replication group.

**Response:**
```json
{
  "message": "group 'llama3.1:70b' deleted"
}
```

#### GET /api/v1/replication/stats

Get extended replication statistics.

**Response:**
```json
{
  "enabled": true,
  "groupCount": 2,
  "stats": [ "...group stats..." ],
  "autoReconcile": true
}
```

#### POST /api/v1/replication/reconcile

Force-trigger reconciliation for all replication groups.

**Response:**
```json
{
  "message": "reconciliation triggered"
}
```

---

### Virtual Models (Virtual Model API)

Manage virtual models (Variant C) — splitting a model into slices and running distributed inference.

#### GET /api/v1/virtualmodels

Get the list of all virtual models.

**Authentication:** Required (API Token)

**Response:**
```json
{
  "enabled": true,
  "count": 2,
  "models": [ "...virtual model objects..." ]
}
```

#### GET /api/v1/virtualmodels/{name}

Get the status of a specific virtual model.

**Response:**
```json
{
  "name": "gpt-large",
  "enabled": true,
  "slices": [...],
  "activeJobs": 3,
  "throughputPerSlice": 12.5
}
```

---

### Autopull (Automatic Model Pull)

Manage automatic model pulling on backends.

#### GET /api/v1/autopull

Get the autopull configuration and status.

**Authentication:** Required (API Token)

**Response:**
```json
{
  "enabled": true,
  "maxConcurrent": 2,
  "pullTimeout": "10m",
  "retryCount": 3,
  "activePulls": [...]
}
```

#### PUT /api/v1/autopull

Update the autopull configuration.

**Request body:**
```json
{
  "enabled": true,
  "maxConcurrent": 2,
  "pullTimeout": "10m",
  "retryCount": 3
}
```

**Response:**
```json
{
  "success": true,
  "config": {},
  "message": "Auto-pull configuration updated successfully"
}
```

#### GET /api/v1/autopull/status

Get the status of active and completed model pulls.

**Response:**
```json
{
  "enabled": true,
  "activePulls": [],
  "totalActive": 0
}
```

---

### CORS (Cross-Origin Resource Sharing)

The API supports CORS for all origins. Additionally, WebSocket uses a whitelist of allowed origins:

| Origin | Allowed |
|--------|---------|
| `http://localhost:3000` | Yes |
| `http://localhost:8080` | Yes |
| `http://localhost:18030` | Yes |
| `http://localhost:18081` | Yes |
| `http://127.0.0.1:18030` | Yes |
| `http://127.0.0.1:18081` | Yes |
| `same-origin` | Yes |

**WebSocket `CheckOrigin`:** When connecting through a browser, the origin is checked against the whitelist. If the origin is not in the whitelist — the connection is rejected.

**HTTP CORS headers (from `ServeHTTP`):**
```
Access-Control-Allow-Origin: *
Access-Control-Allow-Methods: GET, POST, PUT, DELETE, OPTIONS
Access-Control-Allow-Headers: Content-Type, Authorization, X-Agent-ID, X-API-Token
```

### Rate Limit

#### GET /api/v1/ratelimit/status

Get the rate limiter status. Does not require authentication.

**Response:**

```json
{
  "tokens": 100.0,
  "max_tokens": 100,
  "refill_rate": 10.0
}
```

---

## WebSocket API

### Real-time Metrics Stream

```
ws://localhost:18081/api/v1/ws/metrics
```

Streams real-time metrics updates as JSON messages:

```json
{
  "type": "metrics_update",
  "timestamp": "2025-01-01T00:00:00Z",
  "data": {
    "backendId": "gpu-1",
    "gpu": { "usagePercent": 45 },
    "vram": { "usagePercent": 62 },
    "activeRequests": 2,
    "rps": 1.5
  }
}
```

---

## Request Examples

### cURL

```bash
# Get cluster state
curl http://localhost:18081/api/v1/cluster

# Add a backend
curl -X POST http://localhost:18081/api/v1/backends \
  -H "Content-Type: application/json" \
  -d '{"id":"gpu-3","host":"192.168.1.102","ollamaPort":11434}'

# Delete a backend
curl -X DELETE http://localhost:18081/api/v1/backends/gpu-3

# Force rebalance queue
curl -X POST http://localhost:18081/api/v1/queue/rebalance
```

### Python

```python
import requests

# Get cluster state
resp = requests.get("http://localhost:18081/api/v1/cluster")
data = resp.json()
print(f"Backends: {len(data['backends'])}")
print(f"Queue size: {data['queue']['current_size']}")

# Add a backend
resp = requests.post("http://localhost:18081/api/v1/backends", json={
    "id": "gpu-3",
    "host": "192.168.1.102",
    "ollamaPort": 11434
})
```

### JavaScript

```javascript
// Get cluster state
const resp = await fetch('http://localhost:18081/api/v1/cluster');
const data = await resp.json();
console.log(`Backends: ${data.backends.length}`);
console.log(`Queue size: ${data.queue.current_size}`);

// Add a backend
await fetch('http://localhost:18081/api/v1/backends', {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({
    id: 'gpu-3',
    host: '192.168.1.102',
    ollamaPort: 11434
  })
});
```

---

## OpenAPI Specification

The full OpenAPI 3.0 specification is available at:

- **YAML:** [openapi.yaml](../openapi.yaml)
- **JSON:** [swagger.json](../swagger.json)

### Using Swagger UI

```bash
# Run Swagger UI locally
docker run -p 8080:8080 -v $(pwd)/docs/openapi.yaml:/openapi.yaml \
  -e SWAGGER_JSON=/openapi.yaml swaggerapi/swagger-ui
```

### Using Swagger Editor

1. Open https://editor.swagger.io
2. Upload the `openapi.yaml` file
3. Browse and test the API

---

## Summary endpoint table

| Method | Endpoint | Description | Authentication |
|--------|----------|-------------|----------------|
| `GET` | `/api/v1/health` | Health check | No |
| `GET` | `/api/v1/cluster` | Cluster status | Yes |
| `GET` | `/api/v1/metrics` | Metrics for all backends | Yes |
| `GET` | `/api/v1/metrics/{id}` | Metrics for a backend | Yes |
| `GET` | `/api/v1/backends` | List backends | Yes |
| `POST` | `/api/v1/backends` | Add a backend | Yes |
| `GET` | `/api/v1/backends/{id}` | Backend information | Yes |
| `PUT` | `/api/v1/backends/{id}` | Update a backend | Yes |
| `POST` | `/api/v1/backends/{id}/reconfigure` | Reconfigure a backend | Yes |
| `GET` | `/api/v1/backends/{id}/launch-config` | Backend launch config | Yes |
| `GET` | `/api/v1/backends/{id}/models` | Models on a backend | Yes |
| `DELETE` | `/api/v1/backends/{id}` | Remove a backend | Yes |
| `GET` | `/api/v1/sessions` | List sessions | Yes |
| `DELETE` | `/api/v1/sessions` | Clear sessions | Yes |
| `DELETE` | `/api/v1/sessions/{id}` | Delete a session | Yes |
| `GET` | `/api/v1/models` | Running models | Yes |
| `GET` | `/api/v1/models/operations` | Active model operations | Yes |
| `POST` | `/api/v1/agents/register` | Register an agent | Yes |
| `POST` | `/api/v1/agents/metrics` | Submit metrics from an agent | Yes |
| `POST` | `/api/v1/agents/heartbeat` | Agent heartbeat | Yes |
| `GET` | `/api/v1/agents/stats` | Agent statistics | Yes |
| `GET` | `/api/v1/agents/{id}` | Agent information | Yes |
| `GET` | `/api/v1/auth/status` | Authentication status | Yes |
| `POST` | `/api/v1/auth/token` | Create a token | Yes (master) |
| `DELETE` | `/api/v1/auth/token` | Revoke a token | Yes (master) |
| `GET` | `/api/v1/queue/stats` | Queue statistics | Yes |
| `GET` | `/api/v1/queue/details` | Queue details (pending + processing) | Yes |
| `GET` | `/api/v1/queue/history` | History of completed requests | Yes |
| `GET` | `/api/v1/predictions` | Predictions for all backends | Yes |
| `GET` | `/api/v1/predictions/{id}` | Prediction for a backend | Yes |
| `GET` | `/api/v1/models/capacity` | Global model capacity | Yes |
| `GET` | `/api/v1/backends/{id}/capacity` | Backend capacity | Yes |
| `PUT` | `/api/v1/backends/{id}/limits` | Update backend runtime limits | Yes |
| `GET` | `/api/v1/cluster/config` | Current cluster configuration | Yes |
| `PUT` | `/api/v1/cluster/config` | Change cluster algorithm/settings | Yes |
| `POST` | `/api/v1/admin/restart` | Restart the balancer | Yes |
| `GET` | `/api/v1/proxy/logs` | Proxied-request logs | Yes |
| `GET` | `/api/v1/candidates` | Candidate groups by model | Yes |
| `GET` | `/monitor` | Monitor HTML page | Yes |
| `GET` | `/api/v1/ratelimit/status` | Rate limiter status | No |
| `GET` | `/api/v1/replication/groups` | List replication groups | Yes |
| `POST` | `/api/v1/replication/groups` | Create a replication group | Yes |
| `GET` | `/api/v1/replication/groups/{modelName}` | Replication group details | Yes |
| `PUT` | `/api/v1/replication/groups/{modelName}` | Update a replication group | Yes |
| `DELETE` | `/api/v1/replication/groups/{modelName}` | Delete a replication group | Yes |
| `GET` | `/api/v1/replication/stats` | Replication statistics | Yes |
| `POST` | `/api/v1/replication/reconcile` | Force reconcile | Yes |
| `GET` | `/api/v1/virtualmodels` | List virtual models | Yes |
| `GET` | `/api/v1/virtualmodels/{name}` | Virtual model status | Yes |
| `GET` | `/api/v1/autopull` | Autopull configuration | Yes |
| `PUT` | `/api/v1/autopull` | Update autopull configuration | Yes |
| `GET` | `/api/v1/autopull/status` | Autopull status | Yes |
| `GET` | `/ws/metrics?token=xxx` | Metrics WebSocket | Yes (token in query) |

---

## Next steps

- [Troubleshooting](../troubleshooting.md) — Troubleshooting
- [Configuration](../configuration.md) — Authentication and TLS configuration
- [Deployment](../deployment.md) — Production deployment