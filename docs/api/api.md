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

# HTTPS (if `tls.enabled=true` in config.json)
# The port is set by `loadBalancer.tlsPort` (HTTPS proxy) and `tlsPort+1`
# (HTTPS management API). config.example.json sets no default — set it
# explicitly, otherwise the server may start on a random port.
https://localhost:<loadBalancer.tlsPort>      # proxy (Ollama API)
https://localhost:<loadBalancer.tlsPort + 1>  # management API
```

See also: `internal/api/tls.go` (`GenerateSelfSignedCert`, `HTTPSRedirectMiddleware`)
and `cmd/balancer/main.go:291-356` (starting the HTTPS servers).

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

### CppWorker: Ollama-compatible fields for `/api/generate` and `/api/ollama/generate`

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

### CppWorker: `/api/generate` statistics

The response contains real metrics:

- `load_duration` — time from model load until generation starts (μs).
- `prompt_eval_count` — number of prompt tokens, counted via the model's tokenizer (`bridge_count_tokens` / `Backend.CountTokens`).
- `eval_count` — number of tokens in the response.
- `total_duration`, `prompt_eval_duration`, `eval_duration` — durations in μs.

If the model returns an empty `response: ""`, CppWorker returns HTTP 500 with an `error` field.

### CppWorker: `/api/chat`

- Supports `messages` with `system`, `user`, `assistant` roles.
- Prompt construction uses `backend.ApplyChatTemplate` (chat template from GGUF); if no template is available, falls back to a naive format (`<|user|>` / `<start_of_turn>user`).
- Supports `stream`, `temperature`, `max_tokens`, `num_ctx`, `options.stop`.
- Returns `message.role="assistant"`, `done=true` in the final chunk.

### CppWorker: protection against double-loading

`POST /api/models/load` checks `backend.GetModel(name)` before calling `LoadModelWithOpts`:

- If the model is already loaded with the same path and parameters (`ContextSize`, `BatchSize`, `GPULayers`, `FlashAttnType`, `NUMA`, `UseMmap`, `TensorSplit`) — returns `status: "already_loaded"` and does not touch VRAM.
- If the path or parameters differ — `UnloadModel` is called first, then `LoadModelWithOpts` (reload-in-place).
- Two concurrent loads of the same model are serialized via `IsModelLoading`; the second one receives a 503 with `loading: true`.

### CppWorker: Tool Calling (Function Calling)

CppWorker supports function calling in OpenAI/Ollama-compatible formats:

- `/api/chat` (Ollama NDJSON) with a `tools: [...]` field in the request.
- `/v1/chat/completions` (OpenAI SSE) with a `tools: [...]` field in the request.

Behavior:

1. When `tools` is present in the request, cppworker extends the system prompt with descriptions of available functions (`buildToolsSystemPrompt` in `cmd/cppworker/tool_calls.go`).
2. After generation, cppworker attempts to parse `tool_calls` from the model's plain-text output via `parseToolCallsFromOutput`.
3. If recognized — returns `finish_reason: "tool_calls"` and `message.tool_calls`.
4. If NOT recognized — returns a normal text response with `finish_reason: "stop"`.

#### Supported tool_call formats from models

cppworker (`parseToolCallsFromOutput`) and the balancer (`detectAndExtractToolCallsFromContent`) recognize the following formats:

| # | Format | Example | Models |
|---|--------|---------|--------|
| 1 | Ollama/JSON array | `[{"id":"call_x","type":"function","function":{"name":"search","arguments":"..."}}]` | Gemma-4, llama.cpp chat template |
| 2 | Hermes / Qwen 2.5 (XML) | `<tool_call>{"name":"search","arguments":{...}}</tool_call>` | NousResearch Hermes, Qwen 2.5 |
| 3 | Llama-3.x python_tag | `<\|python_tag\|>{"name":"search","parameters":{...}}<\|eom_id\|>` | Llama-3.1+, Meta Llama-3 instruct |
| 4 | Mistral Nemo | `[TOOL_CALLS][{"name":"search","arguments":{...}}]` | Mistral Nemo, Mistral-7B-instruct |
| 5 | JSON inside markdown | ``` ```json [...] ``` ``` | Universal |
| 6 | Single-object JSON | `{"name":"search","arguments":{...}}` | Custom models |
| 7 | JSON with prefix | `Reasoning... [{"id":"call_x",...}]` | Models with chain-of-thought |

Recognition follows priority: first Hermes, then Llama-3, then Mistral, then standard JSON array, then single-object.

#### `tools` fields (OpenAI/Ollama)

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

#### Follow-up messages from tool results

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

#### Streaming with tools

For streaming (`stream: true`) when `tools` is present (Ollama format `/api/chat`):

- cppworker streams tokens as usual via `generateStreamWithRamFallback` (real-time feedback).
- In parallel, it accumulates the full output in a buffer.
- After generation completes, it checks for `tool_calls` via `parseToolCallsFromOutput`.
- If `tool_calls` are detected — it clears `content` and emits a final NDJSON chunk with `done: true`, `done_reason: "tool_calls"`, `message.tool_calls`.
- If NOT detected — it emits a normal chunk with `done_reason: "stop"`.

> **Note:** Previously, when `stream=true && tools!=[]`, cppworker returned non-streaming JSON via `writeJSON`, which broke OpenWebUI (it expected NDJSON chunks and interpreted the response as "source used" with no visible answer). This is now fixed via `writeChatStreamResponseWithTools` in `cmd/cppworker/handlers_chat.go`.

For OpenAI-format `/v1/chat/completions` streaming with tools, `writeToolCallsStream` or `writeStaticTextStream` (SSE) is used.

The balancer (`internal/balancer/llamacpp_translate_resp.go`) additionally tries to parse `tool_calls` from SSE chunk content via `detectAndExtractToolCallsFromContent` to recover structured tool_calls even when cppworker did not recognize them.

#### Known limitations

- Ollama Modelfile templates (`template`, `system` in `.gguf.json`) are not applied — cppworker uses the chat template from GGUF or a naive fallback.
- Ollama registry (`ollama pull llama3`) is not supported; to load models use `hf:<repo>/<file.gguf>` via `/api/pull` or `/api/hf/download`.
- Vision/multimodal tools (image inputs) are not supported — the model receives only the text part.

### CppWorker: per-model profiles (n_ctx management)

**Full documentation:** [cppworker-model-params.md](../features/cppworker-model-params.md).

These endpoints let you set `n_ctx` and other inference parameters for a specific model on cppworker. Used to solve the `n_ctx overflow` problem in Cline/OpenWebUI.

| Endpoint | Method | Description | Auth |
|----------|--------|-------------|------|
| `/api/v1/cppworker/model-profiles` | `GET` | List all per-model profiles | ✅ |
| `/api/v1/cppworker/model-profiles/{name}` | `GET` | Get profile for a model | ✅ |
| `/api/v1/cppworker/model-profiles/{name}` | `PUT` | Create/update profile + save to config.json | ✅ |
| `/api/v1/cppworker/model-profiles/{name}` | `DELETE` | Delete profile | ✅ |
| `/api/v1/cppworker/model-profiles/{name}/apply` | `POST` | Save + reload model on all llama_cpp backends | ✅ |

**Why the window is smaller than the model can do (R267 + R319).** Both `GET` responses
carry the computed reason next to the profile, so nobody has to look for it in the
compose file:

```json
{
  "models": { "gemma-4-E4B-it-Q4_K_M": { "contextLength": 32768, ... } },
  "effectiveContext": {
    "gemma-4-E4B-it-Q4_K_M": {
      "effectiveContextLength": 32768,
      "limitedBy": "env",
      "limitReason": "model window is 16384, but LB_NCTX_RELOAD_MAX_N_CTX=32768 allows auto-growth only up to it",
      "envCeiling": 32768
    }
  },
  "nctxCeiling": {
    "envCeiling": 32768,
    "limitedBy": "env",
    "reason": "auto-growth ceiling LB_NCTX_RELOAD_MAX_N_CTX=32768; a window declared in the model profile may be higher — then the profile limits it, not env"
  }
}
```

`effectiveContext` is built only from EXISTING profiles; `nctxCeiling` is a cluster-wide
value and is always present (even with no profiles). `envCeiling: 0` means "no auto-growth
ceiling": only physical limits (`feasible_max_context` / `gguf_max_context`) apply, checked
at load time.

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

### CppWorker: Admin / Diagnostics API

These endpoints live **on cppworker itself** (port 18092), not on the balancer.
They are intended for diagnostics and runtime control — usually used by the GGUF
WebUI page or admin scripts, not by public clients.

| Endpoint | Method | Description | Auth |
|----------|--------|-------------|------|
| `/health` | `GET` | Health check (Liveness probe) | ❌ |
| `/api/health` | `GET` | Alias for `/health` | ❌ |
| `/info` | `GET` | Version, env, build-info | ❌ |
| `/api/info` | `GET` | Alias for `/info` | ❌ |
| `/api/gpu` | `GET` | GPU info (NVML) | ❌ |
| `/api/models` | `GET` | List of loaded models (`{count, models:[...]}`) | ❌ |
| `/api/model` | `GET` | Get info about a specific model (via `?name=`) | ❌ |
| `/api/models/files` | `GET` | List of `.gguf` files in `modelsDir` + live inventory: `scannedAt`, `changedAt`, `added`, `removed`, `resized`, `dirError`, `rescanTtlSec`, `scans`, `changes`. Rescans the directory on a TTL (`CPPWORKER_MODELS_RESCAN_SEC`, default 30 s); `?refresh=1` — force. An unreachable directory yields `count=0` + `dirError` (not a stale cache) | ❌ |
| `/api/models/refresh` | `POST` | Force a scan of the model directory on disk (same response as `/api/models/files`). Used by the "Check availability" button in the WebUI and by the balancer's forced catalog refresh | ❌ |
| `/api/models/load` | `POST` | Load a model | ❌ |
| `/api/models/load/progress` | `GET` | Load progress (polling) | ❌ |
| `/load` | `POST` | Alias for `/api/models/load` (used by balancer warmup) | ❌ |
| `/api/models/unload` | `POST` | Unload a model | ❌ |
| `/api/models/reload` | `POST` | Unload + load with new parameters | ✅ API_TOKEN |
| `/api/models/delete` | `POST` | Delete a GGUF file from disk | ❌ |
| `/api/delete` | `POST` | Ollama-compatible alias for `delete` | ❌ |
| `/api/v1/cppworker/config` | `GET` | Current config (env+defaults merged) | ❌ |
| `/api/v1/cppworker/config/update` | `POST` | Apply override parameters | ✅ API_TOKEN |
| `/api/v1/cppworker/config/reload` | `POST` | Reload config | ✅ API_TOKEN |
| `/api/v1/cppworker/config/runtime` | `GET` | Runtime parameters of loaded models (n_ctx, gpu_layers, etc.) | ❌ |
| `/api/v1/cppworker/reset-reload-counter` | `POST` | Reset `ramFallbackAttempts` (cycle counter) | ✅ API_TOKEN |
| `/api/v1/cppworker/health` | `GET` | Health check (v1 namespace) | ❌ |
| `/api/v1/cppworker/metrics` | `GET` | Metrics (Prometheus-compatible) | ❌ |
| `/api/v1/cppworker/debug/last-prompt` | `GET` | Last prompt that caused a 413 | ✅ API_TOKEN |
| `/api/v1/cppworker/debug/last-stream` | `GET` | Snapshot of the last stream (for diagnosing disconnects) | ✅ API_TOKEN |
| `/api/v1/cppworker/debug/last-stream/clear` | `POST` | Clear the snapshot | ✅ API_TOKEN |
| `/api/diagnostics` | `GET` | Full diagnostic JSON | ❌ |
| `/api/diagnostics/models` | `GET` | Per-model diagnostics | ❌ |
| `/api/diagnostics/load` | `GET` | Diagnostics of the last load | ❌ |
| `/api/diagnostics/clear` | `POST` | Clear diagnostics | ❌ |

See `cmd/cppworker/router.go` for the registration of all routes.

## Anthropic Messages API (via the balancer)

R247 (2026-10-26): Anthropic-native clients (Claude Code, `anthropic` SDK) are served by the balancer. The handler translates to the OpenAI surface internally and back, so tools, strict schemas, tool history, retries and admission rules work exactly as on `/v1/chat/completions`.

| Endpoint | Method | Description |
|---|---|---|
| `/v1/messages` | `POST` | Anthropic Messages API (stream and non-stream) |
| `/v1/messages/count_tokens` | `POST` | Conservative token estimate (`X-Token-Count-Estimate: chars/3`) |

**Translated fields:** `system` (string or text blocks), `messages[].content` blocks
(`text`, `image`, `tool_use`, `tool_result`), `tools[].input_schema` -> `parameters`,
`tool_choice` (`auto` -> `auto`, `any` -> `required`, `tool{name}` -> forced function),
`max_tokens`, `stop_sequences`, `temperature`, `top_p`.

**Response:** `content[]` blocks `text` / `tool_use` (`input` is always an object),
`stop_reason` (`end_turn` / `tool_use` / `max_tokens`), `usage`
(`input_tokens` / `output_tokens`).

**Stream events (Anthropic order):** `message_start`, `content_block_start`
(`text` or `tool_use`), `content_block_delta` (`text_delta` / `input_json_delta`),
`content_block_stop`, `message_delta` (`stop_reason` + `output_tokens`), `message_stop`.

**Errors:** `{"type":"error","error":{"type":...,"message":...}}` with types
`invalid_request_error`, `request_too_large`, `overload_error`, `api_error`.

**Not translated (by design):** `thinking` blocks with `signature`, `cache_control`,
and `input_tokens` in `message_start` is sent as `0` (upstream usage arrives later in
`message_delta`). Live contract: `scripts/webui-e2e/client-expectations-r247.js`
profiles `anthropic-sdk` and `anthropic-error-shape`.

## OpenAI Responses API (via the balancer)

R247 (2026-10-26): Codex-style clients that call only `/v1/responses` are served by the balancer. Same approach as the Anthropic surface: translated to `/v1/chat/completions` internally and back, so tools, strict schemas, tool history, retries and admission behave exactly as on the chat surface.

| Endpoint | Method | Description |
|---|---|---|
| `/v1/responses` | `POST` | OpenAI Responses API (stream and non-stream) |

**Request mapping:** `instructions` -> system message; `input` as a string or as items
(`message`, `function_call` -> assistant `tool_calls`, `function_call_output` -> `role:"tool"`);
`tools[]` flat form (`{type:"function", name, description, parameters, strict}`) ->
`tools[].function`; `tool_choice` (`auto`/`none`/`required` or `{type:"function", name}`
-> forced function); `max_output_tokens` -> `max_tokens`; `temperature`, `top_p`, `stream`.

**Response:** `{id: "resp_…", object: "response", status: "completed", output[], output_text, usage}`
where `output[]` holds `message` items with `content[] = [{type:"output_text", text, annotations}]`
and `function_call` items (`call_id`, `name`, `arguments` as a **string**).

**Stream events:** `response.created`, `response.output_item.added`,
`response.content_part.added`, `response.output_text.delta` / `.done`,
`response.content_part.done`, `response.function_call_arguments.delta` / `.done`,
`response.output_item.done`, `response.completed`, each with a monotonic `sequence_number`.

**Errors:** `{"error":{"message","type","param","code"}}` with `type = invalid_request_error`
or `api_error`.

**Not supported (honest refusal):** `previous_response_id` returns HTTP 400 with an
explanation — dialog state is not stored, send the full `input` history instead.
Live contract: `scripts/webui-e2e/client-expectations-r247.js` profiles
`codex-responses` and `responses-error-shape`.

## GGUF Backend Proxy API (via the balancer)
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

### Features

- **Transparent proxying**: HTTP method, headers (including `X-HF-Token`), query string and JSON body are forwarded to CppWorker unchanged.
- **Automatic host resolution**: if a backend is registered with `host=host.docker.internal`, the proxy replaces it with `localhost` (since `host.docker.internal` does not resolve in the browser).
- **Timeout**: 90 seconds (HF search/download can take up to 60-90s, especially search with parallel file fetching).
- **Clear error messages**: on network issues, `502 Bad Gateway` / `504 Gateway Timeout` is returned with the CppWorker address.

### cURL examples

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

### Response codes

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

### Detailed description of Ollama endpoints

#### GET /api/tags

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

#### GET /api/ps

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

#### GET /api/version

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

#### POST /api/show

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

#### POST /api/create

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

#### POST /api/pull

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

#### DELETE /api/delete

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

#### POST /api/copy

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

#### POST /api/push

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

### Data format

- **Request**: JSON
- **Response**: JSON
- **Content-Type**: `application/json`

### Response codes

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
| `GET /api/v1/health` | — | ❌ No | Public, used by WebUI to detect settings |
| `GET /api/v1/ratelimit/status` | — | ❌ No | Public |
| `GET /api/v1/auth/status` | — | ✅ Yes | Requires token |
| `POST /api/v1/auth/token` | — | ✅ Master only | Generate a new token |
| `GET /api/v1/cluster` | — | ✅ Yes | |
| `GET /api/v1/backends` | — | ✅ Yes | |
| `POST /api/v1/backends` | — | ✅ Yes | |
| `GET /api/v1/metrics` | — | ✅ Yes | |
| `GET /api/v1/models` | — | ✅ Yes | |
| `GET /api/v1/sessions` | — | ✅ Yes | |
| `GET /api/v1/agents/stats` | — | ✅ Yes | |
| `GET /api/v1/queue/stats` | — | ✅ Yes | Request queue statistics |
| `GET /api/v1/models/catalog` | — | ✅ Yes | Catalog of models **on disk** per llama.cpp backend: `files`, `fetchedAt`, `ageSec`, `changedAt`, `added`, `removed`, `dirError`, `known`, `refreshIntervalSec`. `?refresh=true` — re-read listings from the workers immediately (regardless of `LB_MODEL_CATALOG_REFRESH_SEC`, including when background refresh is off). Empty `files` with `known=true` = "no models on disk"; `dirError` = "the worker's directory is unreachable" |
| `WebSocket /ws/metrics` | — | ✅ If auth enabled | Token passed via query parameter |

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

> **💡 How WebUI works:** When the dashboard loads, it first calls `GET /api/v1/health` **without a token**. If `authEnabled: false` — the application starts immediately. If `authEnabled: true` — a modal window is shown for token entry.

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
      "host": "192.0.2.66",
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
  "host": "192.0.2.80",
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
      "client_ip": "192.0.2.100"
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

---

### Agents (Registration & Metrics)

#### POST /api/v1/agents/register

Register an agent in the balancing system. The agent sends this request at startup to auto-register with the cluster.

**Authentication:** Required (API Token)

**Request Body:**
```json
{
  "agentId": "gpu-1",
  "hostname": "gpu-server-1",
  "host": "192.0.2.100",
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
    "host": "192.0.2.100",
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
    "host": "192.0.2.100",
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

**Request Body — full agent metrics structure:**

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

### Predictions (Forecasting)

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

#### POST /api/v1/agents/heartbeat

Send a heartbeat from the agent to confirm liveness.

**Authentication:** Required (API Token)

**Headers:**

| Header | Value | Description |
|--------|-------|-------------|
| `X-Agent-ID` | string | Agent ID (required) |

**Request Body:** Not required (may be empty)

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

##### The `image` block (R98 / S8 of the 2026-10-07 plan)

Appears when the installation has image backends (`image_cpp`) or there has been image
traffic. On a text-only installation the key is absent: a zero image block would look
like "the image path is in use".

```json
{
  "image": {
    "in_flight": 1,
    "total": 12,
    "ok": 10,
    "failed": 1,
    "rejected": 0,
    "accepted": 1,
    "finished": 11,
    "rps": 0.05,
    "avg_duration_ms": 4800,
    "backends": ["img-1", "img-2"],
    "backends_in_flight": { "img-1": 1 },
    "recent": [
      { "id": "img-7", "backendId": "img-1", "model": "sd:flux", "path": "/v1/images/generations",
        "batch": 1, "status": "accepted", "width": 1024, "height": 1024, "steps": 20 }
    ]
  }
}
```

| Field | Meaning |
|---|---|
| `in_flight` | How many generations are running right now (**int**, not int64 — as in `/api/v1/metrics`) |
| `total` / `ok` / `failed` / `rejected` | Started / succeeded / failed / rejected by the VRAM gate (do not add them up: `accepted` and `finished` are subsets) |
| `rps` | Requests per second over a sliding 60 s window |
| `avg_duration_ms` | Average duration of completed requests, ms |
| `backends` | ids of the registered image backends (deterministic order) |
| `backends_in_flight` | How many generations per node: without it only the total "in flight" is visible, and it is unclear which node to wait for |
| `recent` | Feed of the latest requests (id, model, dimensions, steps, status) — the record appears right at the start, not only at the finish: a long generation would otherwise look like silence |

The same keys are returned by `/api/v1/metrics` in the `image` block (a common source —
`imageRequestStore`), plus a full per-backend breakdown is available there.

##### Image batches: `n > 1` is spread across nodes (R99 / S8 of the 2026-10-07 plan)

sd.cpp cannot split ONE generation across GPUs, but `n` images are `n`
**independent** tasks: the balancer runs them in parallel on different image backends and
stitches the response together. This is the only real speedup for imageworker without
forking the engine.

| Surface | Number of images | Spread out |
|---|---|---|
| `POST /v1/images/generations` | `n` | yes |
| `POST /sdapi/v1/txt2img` | `batch_size` × `n_iter` | yes |
| `POST /api/image/generate` | `batch_count` | **no** — this queues an asynchronous job (202 + job id), it has no "common response" made of images |
| `POST /v1/images/edits`, `/variations` | — | **no** — multipart form and different `n` semantics |

Spreading conditions: more than one image, at least two `image_cpp` nodes, and
**the VRAM gate with its lock is taken per node** (`beforeGeneration`). A node whose gate
refused (busy, not enough VRAM) does not take part in the distribution; if fewer than two
nodes are ready — the usual single path runs and behavior does not change.

The response is the usual surface response, stitched from the task responses (for OpenAI
`data[]` is merged, for A1111 — `images[]`; `parameters`/`info` are taken from the first
task, because they describe the request, not the image). The order of images = the order
of tasks. Additional headers:

| Header | Meaning |
|---|---|
| `X-LB-Image-Batch` | `2 backends / 4 images` — the distribution happened |
| `X-LB-Image-Batch-Parts` | `img-a=2,img-b=2` — who computed how many |
| `X-LB-Image-Batch-Failed-Backend` | the node that made the batch fail |

**The failure is honest, not partial:** if at least one task fails, the client receives
an error naming the node (and the engine's original text), not "3 images instead of 4".
Each task is recorded in the statistics separately — visible in `/api/v1/queue/stats` and
`/api/v1/metrics` (the `image` block), so on a live installation you can verify that the
batch really was spread out.

> **💡 How dispatch works:**
> 1. **Model Affinity** — the request goes to a backend where the model is already loaded (stages 1–3 in `selectBackend`)
> 2. **Resource-Aware** — selection by free resources (GPU, VRAM, CPU) with a prediction bonus
> 3. **Config/Weight** — fallback selection by backend weights (when `UseEnhancedScoring=false`)

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

### Backend Groups (logical backends, R92 / S2 of the 2026-10-07 plan)

A logical backend is a group of physical nodes across which **one** model is split
(master + RPC workers). For the balancer such a group is ONE backend: one
health, one queue slot, one `X-Backend-ID`. This is not replication: a copy
requires the model to fit entirely into every node, while a group exists precisely
because it does not fit entirely into any single one.

Configuration — `balancing.backendGroups[]`:

```json
{
  "balancing": {
    "backendGroups": [
      {
        "id": "g1",
        "model": "qwen3-14b*",
        "strategy": "layer",
        "reason": "14B does not fit into 8 GB of a single card",
        "members": [
          { "backendId": "cppworker-a", "role": "master" },
          { "backendId": "cppworker-b", "role": "worker", "rpcEndpoint": "192.0.2.11:50052" }
        ]
      }
    ],
    "placement": {
      "enabled": true,
      "models": [{ "model": "qwen3-14b*", "strategy": "sharded" }]
    }
  }
}
```

Validation rules (`types.ValidateBackendGroups`, visible in the warnings at
load time and in `GET /api/v1/placement`): a unique `id`, a non-empty `model`
mask, `strategy` ∈ `layer|row`, exactly one `master`, at least two members, and
every worker must have an `rpcEndpoint`.

#### `GET /api/v1/placement` — the `groups` block

The state of each group: members with roles, health of each node, the reason for
degradation, total free VRAM and slots.

```json
{
  "groups": [
    {
      "id": "g1",
      "model": "qwen3-14b*",
      "strategy": "layer",
      "masterBackendId": "cppworker-a",
      "members": [
        { "backendId": "cppworker-a", "role": "master", "registered": true, "healthy": true,
          "vramFreeMB": 3000, "vramTotalMB": 8192, "vramKnown": true },
        { "backendId": "cppworker-b", "role": "worker", "rpcEndpoint": "192.0.2.11:50052",
          "registered": true, "healthy": true, "vramFreeMB": 5000, "vramTotalMB": 8192, "vramKnown": true }
      ],
      "vramFreeMB": 8000,
      "vramTotalMB": 16384,
      "vramKnownMembers": 2,
      "slots": 1,
      "loadedModels": ["gemma-3-4b", "qwen3-14b"],
      "healthy": true,
      "degraded": false,
      "usable": true
    }
  ],
  "backendGroupWarnings": [],
  "placement": { "enabled": true, "fallback": "error", "models": [] },
  "backendGroups": []
}
```

With `R102`, GET additionally returns the **raw** `placement` and `backendGroups` (what
`PUT` accepts), and with `R104` — `loadedModels` in the state of the group and its nodes: on
"Split" these models will be unloaded, and the merge window shows the list BEFORE
confirmation instead of leaving the operator to find out from the timeout of the next
request.

Field semantics:

| Field | Meaning |
|---|---|
| `healthy` | Whether the **master** is alive — "a group is alive as long as the master is alive" |
| `degraded` | The layout is not fully executable: the master OR at least one worker dropped out |
| `usable` | `healthy && !degraded` — only such a group routes requests |
| `vramFreeMB` / `vramTotalMB` | **Sum** across nodes: with layer/row split the layers live on different cards |
| `vramKnownMembers` | How many nodes reported VRAM metrics (numbers are not invented) |
| `slots` | Slots of the **master**, not the sum across nodes (otherwise the queue would overestimate capacity) |
| `loadedModels` | What is loaded on the group's nodes (R104): on "Split" this is exactly what will be unloaded. Also present on each `members[]` |

The policy decision for a model additionally contains `groupId` and `groupMembers`.
If the group is not ready, the decision is marked `degraded: true` with numbers in `reason`
(how much is needed and how much is available per node), and then the general mechanism of §6
takes over: `fallback=error` → HTTP 503 with the `placement` block, `fallback=single` → service
by a single backend with the `X-LB-Placement-Fallback: sharded` header.

#### `GET /api/v1/backends` — the `groups` key

The same list of group states (when at least one group exists). A separate
key, so as not to change the format of the `backends` array: the physical nodes of a group
remain ordinary entries there.

#### `PUT /api/v1/placement` — apply policy and logical backends (R100 / S5-S6)

"Writing `balancing.placement`": before R100 the server could only propose (see `plan`
and `validate` below), and there was nothing to apply the result with — the settings master and
the merge window ran into manual editing of `config.json`.

```json
{
  "placement": {
    "enabled": true, "fallback": "error",
    "models": [{ "model": "qwen3-14b*", "strategy": "sharded", "reason": "layout across two machines" }]
  },
  "backendGroups": [{
    "id": "g1", "model": "qwen3-14b*", "strategy": "layer",
    "members": [
      { "backendId": "cppworker-a", "role": "master" },
      { "backendId": "cppworker-b", "role": "worker", "rpcEndpoint": "192.0.2.11:50052" }
    ]
  }],
  "dryRun": false,
  "allowWarnings": false
}
```

Behavior:

| Aspect | How it works |
|---|---|
| **Full replacement, not a merge** | The submitted `placement` and `backendGroups` replace the sections entirely — otherwise deletion (removing a rule, "Splitting" a group) could not be expressed |
| Validation before applying | The same checks as for the config at load time (`PlacementSettings.Validate` + `ValidateBackendGroups`). An error → HTTP 400, `applied: false`, **nothing changes** |
| `allowWarnings: true` | Apply despite warnings (for example the legacy `rpc` strategy). The remarks are still listed in the response |
| `dryRun: true` | Validate only: `valid`, `warnings`, `applied: false`, the config is not saved |
| Applying | Immediately to the live balancer (policy + group registry), then writing the file — like other admin operations |
| `persisted` / `persistError` | In the bundled stack `config.json` is mounted read-only: the policy **takes effect**, but `persisted: false` and the reason in `persistError`. After a restart the installation returns to the file — the response says so directly |

Response (200):

```json
{
  "success": true, "applied": true, "persisted": true,
  "warnings": [],
  "placement": { "enabled": true, "fallback": "error", "models": ["…"] },
  "groups": [ { "id": "g1", "usable": false,
                "degradeReason": "node cppworker-b (worker) is not registered with the balancer" } ],
  "persistError": "",
  "hint": "the policy was applied to the live balancer; if persisted=false the installation will return to the config file after a restart"
}
```

The group states are returned **after** applying — they show whether the
layout actually came together, not merely whether it was written to the file.

`GET /api/v1/placement` — state, `PUT` — apply; other methods → 405.

Since `R102` GET also returns the **raw sections** — `placement` (`models[]`/`classes[]`
rules as they are written) and `backendGroups` (the group config). This is needed by the editing
UI: PUT replaces the sections entirely, so the merge window must carry over other people's rules and
groups as they are. In `decisions` there is no data for that — it holds the RESULT of resolution, from which
it is impossible to recover what the operator wrote. The "GET → add your own → PUT" round is verified by
the test `TestPlacementEndpoint_RawConfigForEditing_R102`.

#### `POST /api/v1/placement/validate` — dry run of a composition (R95 / S4)

Validates the PROPOSED cluster composition **before saving** and changes nothing
(the installation rule: WebUI only proposes). The checks are §7 of the
2026-10-07 plan: reachability of nodes and RPC ports, llama.cpp version compatibility,
the sum of free VRAM of the nodes against the weights with a 25 % margin, memory for
the KV cache at the given `nCtx`, the layout traffic estimate from §3, the "single suffices"
hint, backend type compatibility, GPU homogeneity.

Request body:

```json
{
  "group": {
    "id": "g1", "model": "qwen3-14b", "strategy": "layer",
    "members": [
      { "backendId": "cppworker-a", "role": "master" },
      { "backendId": "cppworker-b", "role": "worker", "rpcEndpoint": "192.0.2.11:50052" }
    ]
  },
  "model": {
    "name": "qwen3-14b", "backendType": "llama_cpp",
    "sizeBytes": 8589934592, "hiddenSize": 4096, "layers": 32,
    "nCtx": 65536, "kvLayers": 32, "kvHeads": 8, "kvHeadDim": 128
  },
  "lang": "ru",
  "bandwidthMBps": 112,
  "prefillTokens": 512,
  "tokensPerSecond": 30,
  "requireHomogeneous": true,
  "skipProbes": false
}
```

Whatever is missing from the request is taken from the live state of the balancer: health, VRAM and RAM
of the nodes (metrics), backend type, the GPU "identity" (labels `gpu:*`/`sm_*`), the model size
(profile/metrics). `bandwidthMBps` defaults to 112 MB/s (1 Gbit/s per §3).

Response:

```json
{
  "success": true,
  "dryRun": true,
  "lang": "ru",
  "groupId": "g1",
  "model": "qwen3-14b",
  "hasErrors": true,
  "errors": [
    {
      "check": "vram_sum_insufficient",
      "level": "error",
      "target": "g1",
      "text": "need ≈10.0 GB (weights 8.0 GB + 25% margin), available 4.0 GB across 2 node(s) (maximum on a single node 2.0 GB)",
      "params": { "needMB": 10240, "availableMB": 4096, "maxNodeMB": 2048, "nodeCount": 2, "safety": 1.25 }
    }
  ],
  "warnings": [],
  "hints": [],
  "issues": [],
  "configWarnings": [],
  "checks": ["node_unreachable", "rpc_unreachable", "version_mismatch", "version_unknown",
             "vram_sum_insufficient", "kv_ram_insufficient", "kv_unknown", "traffic_row_high",
             "fits_single", "backend_type_mismatch", "homogeneous"],
  "hint": "dry run: nothing was saved or changed — apply the setting with a separate config operation"
}
```

Remark fields:

| Field | Meaning |
|---|---|
| `check` | Stable check identifier (`CheckID`) — the UI draws its own labels from it plus `params` |
| `level` | `error` (must not assemble) \| `warning` (may, but deliberately) \| `hint` (a hint) |
| `target` | What it refers to: a node id or a group id |
| `text` | Ready-made text in the `lang` language (ru/en) — for logs, CLI and the API response "as is" |
| `params` | NUMBERS (MB, bytes, percent, nCtx) for machine processing |

What the validator does **not** invent:

- **llama.cpp version.** A node does not report it today (it is neither in the agent
  heartbeat nor in cppworker metrics), so instead of a false error a `warning`
  `version_unknown` is returned — "RPC protocol compatibility is not confirmed". The field itself
  will appear together with the real transport (S1).
- **KV.** Without `kvLayers`/`kvHeads`/`kvHeadDim` the `nCtx` check is marked with a
  `hint` `kv_unknown` rather than being considered passed.
- **Traffic.** Without a measured `bandwidthMBps`/`tokensPerSecond` the absolute latency
  is computed, but the token time share is not (`-1` in the estimate).

`configWarnings` — structural errors of the group itself (no master, a worker without
`rpcEndpoint`, a single node). They come as strings from the Russian config validator
(`types.ValidateBackendGroups`); bringing them into the same localized catalog is
the work of S5, where the UI will start drawing labels by `check`.

---

#### `POST /api/v1/placement/plan` — proposed placement plan (R96 / S5)

The server side of the "cluster composition instead of choosing a mode" step: the master shows
which model lands on what, **with numbers**, and receives a ready-made config fragment.
It applies nothing (the installation rule: WebUI only proposes).

Request body (all fields except `models[].name` are optional):

```json
{
  "models": [
    { "name": "qwen3-14b", "sizeBytes": 8589934592, "hiddenSize": 4096, "layers": 32, "nCtx": 32768 }
  ],
  "lang": "ru",
  "bandwidthMBps": 112,
  "prefillTokens": 512,
  "tokensPerSecond": 30,
  "requireHomogeneous": false,
  "allowDegraded": false,
  "singleOnly": false
}
```

An empty `models` means "plan all models known to the installation" (`llamaCppModelProfiles`
profiles + the catalog of GGUF files on the nodes, R91); their number comes in
`discoveredModels`. A model size not given in the request is taken from the profile or
from the metrics of the loaded model.

How the strategy is chosen (matrix of §8 of the plan):

| Condition | Proposal |
|---|---|
| model size unknown | `single` + `degraded` + the hint "no profile/metrics" |
| fits into ≥2 nodes (and `singleOnly=false`) | `replicated`: copies give a multiple of throughput |
| fits into exactly one node | `single` |
| does not fit into any one but fits into the **sum** of ≥2 nodes | `sharded` + the proposed group (master — the node with the most free VRAM) |
| does not fit anywhere | `single` + `degraded` + numbers (needed / maximum / total) |

Response:

```json
{
  "success": true,
  "dryRun": true,
  "lang": "ru",
  "discoveredModels": 2,
  "plans": [
    {
      "model": "qwen3-14b", "strategy": "sharded", "source": "proposal",
      "reason": "no single node holds the model (need 10.0 GB, maximum free 6.0 GB), but the sum across 2 nodes (12.0 GB) does: layer sharding",
      "groupId": "group-qwen3-14b",
      "backendGroup": {
        "id": "group-qwen3-14b", "model": "qwen3-14b", "strategy": "layer",
        "members": [
          { "backendId": "cppworker-a", "role": "master" },
          { "backendId": "cppworker-b", "role": "worker", "rpcEndpoint": "192.0.2.11:50052" }
        ]
      },
      "traffic": {
        "layer": { "bytesPerToken": 8192, "tokenOverheadMs": 0.073, "tokenOverheadPercent": 0.22, "prefillMs": 37.4, "known": true },
        "row":   { "bytesPerToken": 262144, "tokenOverheadMs": 2.34, "tokenOverheadPercent": 7.03, "prefillMs": 1198, "known": true }
      },
      "issues": [],
      "notes": ["the workers' RPC port is proposed by default (50052) — verify it in the validator"],
      "rule": { "model": "qwen3-14b", "strategy": "auto",
                "auto": { "prefer": ["sharded", "single"], "allowDegraded": false } },
      "sizeGB": 8.0, "needGB": 10.0, "maxNodeGB": 6.0, "sumFreeGB": 12.0,
      "knownSize": true, "degraded": false
    }
  ],
  "placement": { "enabled": true, "fallback": "error", "models": ["…rule…"] },
  "backendGroups": ["…backendGroup…"],
  "warnings": [],
  "hint": "This is a proposal, not an application: the policy is written by a separate config operation after confirmation."
}
```

What matters in the semantics:

- **`rule.strategy = "auto"`, not a "frozen" strategy.** The cluster composition and
  free VRAM change, so `auto` is written into the policy with
  `prefer: [<proposed>, "single"]`: today it will pick exactly what the operator
  saw in the plan, and tomorrow — whatever matches the installation.
- **`traffic` is computed only for `sharded`** (`single`/`replicated` have no
  inter-node network exchange) and for both strategies at once — the operator
  chooses between `layer` and `row` by numbers, not "by eye".
- **`issues`** — the same validator of §7 (`POST /api/v1/placement/validate`)
  applied to the proposed layout.
- **Busy GPUs are not reused**: nodes already given to workers of
  existing groups, and image backends, do not get into the proposal.
- **`warnings`** — structural errors of the proposed groups (for example a missing
  `rpcEndpoint` when the node's host is unknown).
- **The worker RPC port is guessed** (`host:50052`) and marked in `notes`:
  only the operator knows the real port, and the validator will check it.

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
| `http://localhost:3000` | ✅ |
| `http://localhost:8080` | ✅ |
| `http://localhost:18030` | ✅ |
| `http://localhost:18081` | ✅ |
| `http://127.0.0.1:18030` | ✅ |
| `http://127.0.0.1:18081` | ✅ |
| `same-origin` | ✅ |

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

## Cluster AutoDistribute API (R59 — R59.2, 2026-09-03)

Automatic distribution of models between backends based on the current
utilization (`busyScore = ActiveRequests/MaxConcurrentReqs`,
`modelCountScore = loaded/MaxModels`). A suggestion = a pair (overloaded,
underloaded) of the same type, at most 3 per source.

Spec: `docs/archive/superpowers/specs/2026-09-03-cluster-autodistribute.md`,
`docs/archive/superpowers/specs/2026-09-03-cluster-autosuggest-apply.md`.

### GET /api/v1/admin/cluster/autosuggest

Get suggestions (read-only, no side effects).

**Response 200 OK:**

```json
{
  "timestamp": "2026-09-03T16:30:00Z",
  "backends": [
    {"id": "b1", "host": "...", "port": 18092, "type": "llama_cpp",
     "status": "healthy", "activeRequests": 3, "maxConcurrentRequests": 4,
     "loadedModelCount": 3, "maxModels": 3, "busyScore": 0.75, "modelCountScore": 1.0,
     "isOverloaded": false, "isUnderloaded": false},
    ...
  ],
  "suggestions": [
    {"id": "sug-1", "type": "move", "fromBackend": "b1", "toBackend": "b2",
     "model": "gemma-4-9b", "priority": 1,
     "reason": "b1 overloaded (busy=0.85), b2 underloaded (busy=0.10)"}
  ],
  "summary": {"totalBackends": 3, "overloadedCount": 1, "underloadedCount": 1, "suggestionCount": 1}
}
```

### POST /api/v1/admin/cluster/autosuggest/apply

Apply suggestions (operator-confirmed). R59.2 — a real wire with
`unload` on the source + `load` on the destination + rollback reload on failure.

**Request:**

```json
{
  "suggestion_ids": ["sug-1", "sug-3"]
}
```

**Response 200 OK:**

```json
{
  "applied": 2,
  "failed": 0,
  "details": [
    {"id": "sug-1", "status": "ok", "movedModel": "gemma-4-9b",
     "fromBackend": "b1", "toBackend": "b2"},
    ...
  ],
  "errors": []
}
```

Errors:
- `404 Not Found` — the suggestion_id does not exist or has already been applied.
- `409 Conflict` — the source/dest backend became unhealthy between suggest and apply.
- `503 Service Unavailable` — the load/unload operation failed, a rollback was applied.

### Live test

`scripts/test_autosuggest_endpoints.sh` — bash smoke test (R59.2):
```bash
bash scripts/test_autosuggest_endpoints.sh
# → GET /api/v1/admin/cluster/autosuggest → 200 OK with valid JSON
# → POST /api/v1/admin/cluster/autosuggest/apply → 200 OK
```

---

## AutoTune API (R54.1 — R55.2, 2026-08-24)

AutoTune is an autonomous mechanism for optimizing the parameters of loaded models:
  - Detects sub-optimal states (n_ctx over-allocation, q4_0 KV cache
    with sufficient VRAM, num_gpu_layers mismatch).
  - Computes optimal params taking the workload into account (R54.9, p95 num_ctx).
  - Auto-applies via async reload (R54.4) if AutoTune is enabled
    (R54.2 global + per-model override).
  - Protected by a circuit breaker (R54.4: 60s cool-down / 300s stable).
  - Live updates via WebSocket (R54.8) + a history log for replay (R55.2).

### `GET /api/v1/admin/autotune` — global state

Returns the AutoTune state for all backends: sub-optimal models, circuit
state, recent history (R55.2). Requires authentication.

**Query params:** none.

**Response (fragment):**

```json
{
  "backends": [
    {
      "id": "cppworker-gpu-bundled-agent",
      "plan": {
        "modelName": "Qwen3-Instruct-2507-q4km",
        "contextSize": 46592,
        "kvCacheType": "f16",
        "numGpuLayers": -1,
        "reason": "R54.4: n_ctx 131072 → 46592 (over-allocation fix)"
      },
      "circuits": {
        "Qwen3-Instruct-2507-q4km": {
          "lastAttempt": "2026-08-24T22:00:00Z",
          "lastSuccess": "2026-08-24T22:00:30Z",
          "canReload": true
        }
      },
      "workloads": {
        "Qwen3-Instruct-2507-q4km": {
          "sampleCount": 23,
          "avgNumCtx": 1850,
          "p95NumCtx": 4096,
          "maxNumCtx": 8192
        }
      },
      "recentHistory": [
        {
          "timestamp": "2026-08-24T22:00:30Z",
          "type": "autotune_reload_succeeded",
          "backendId": "cppworker-gpu-bundled-agent",
          "model": "Qwen3-Instruct-2507-q4km",
          "severity": "info",
          "message": "AutoTune reload succeeded: n_ctx 131072 → 46592",
          "data": {"contextSize": 46592, "kvCacheType": "f16"}
        }
      ]
    }
  ]
}
```

### `POST /api/v1/admin/autotune/{backendID}/apply` — manual apply

Applies the AutoTune recommendations for a specific backend. Async — 202
accepted, the actual reload happens in the background. Requires authentication.

**Body:**

```json
{
  "modelName": "Qwen3-Instruct-2507-q4km",
  "forceApply": false
}
```

**Response (200/202):**

```json
{
  "backendId": "cppworker-gpu-bundled-agent",
  "result": {
    "triggered": true,
    "skippedReason": "",
    "circuitState": {"canReload": true}
  },
  "message": "AutoTune apply scheduled for Qwen3-Instruct-2507-q4km"
}
```

### `GET /api/v1/admin/autotune/history` — event log (R55.2)

A ring buffer of the last 500 AutoTune events (triggered / succeeded /
failed / circuit_open). Newest first. Used by the WebUI for
timeline visualization — "what AutoTune did over the last N hours".
Requires authentication.

**Query params:**
  - `limit=N` — max entries (default 100, max 500)
  - `since=<RFC3339>` — only entries with timestamp > since
  - `backend=<id>` — filter by backendId

**Example:**

```bash
curl -H "X-API-Token: changeme-bundled-with-agent-token" \
  "http://localhost:18081/api/v1/admin/autotune/history?limit=10&backend=cppworker-gpu-bundled-agent"
```

**Response:**

```json
{
  "entries": [
    {
      "timestamp": "2026-08-24T22:00:30Z",
      "type": "autotune_reload_succeeded",
      "backendId": "cppworker-gpu-bundled-agent",
      "model": "Qwen3-Instruct-2507-q4km",
      "severity": "info",
      "reason": "autotune",
      "message": "AutoTune reload succeeded: n_ctx 131072 → 46592",
      "data": {"contextSize": 46592, "kvCacheType": "f16", "numGpuLayers": -1}
    }
  ],
  "stats": {"size": 1, "maxSize": 500, "droppedCount": 0}
}
```

### `GET /api/v1/admin/autotune/config` — config (R54.7)

Returns the current AutoTune state: global switch + per-model
overrides. Used by the Settings page.

### `PUT /api/v1/admin/autotune/config` — update config

Updates the AutoTune config. Body:

```json
{
  "globalEnabled": true,
  "perModel": { "qwen3-4b": false }
}
```

---

## WebSocket API

### GET /ws/metrics

WebSocket connection for receiving real-time metrics.

**Protocol:** WebSocket
**Message format:** JSON
**Authentication:** A token is required in the query parameter `?token=xxx`

### Authentication

The token must be passed in the URL query parameter. This is necessary to protect against DoS attacks, since the authentication check is performed BEFORE the WebSocket upgrade.

**Important:** If the token is missing or invalid, the server returns an HTTP 401 error before the WebSocket connection is established.

### Connection

```javascript
// Pass the token via a query parameter
const ws = new WebSocket('ws://localhost:18081/ws/metrics?token=your-api-token');

ws.onopen = () => {
  console.log('Connected to metrics stream');
};

ws.onmessage = (event) => {
  const metrics = JSON.parse(event.data);
  console.log('Received metrics:', metrics);
  
  // metrics contains:
  // - id: backend identifier
  // - timestamp: metrics collection time
  // - gpu: GPU metrics (usagePercent, memoryTotal, memoryUsed, temperature, etc.)
  // - system: system metrics (cpuUsagePercent, memoryTotal, memoryUsed, etc.)
  // - ollama: Ollama metrics (runningModels, activeRequests, requestsPerSecond, etc.)
};

ws.onerror = (error) => {
  console.error('WebSocket error:', error);
};

ws.onclose = () => {
  console.log('Connection closed');
};
```

### Connection examples

```javascript
// Browser JavaScript
const token = 'your-api-token';
const ws = new WebSocket(`ws://localhost:18081/ws/metrics?token=${token}`);

// Node.js with the ws library
const WebSocket = require('ws');
const token = 'your-api-token';
const ws = new WebSocket(`ws://localhost:18081/ws/metrics?token=${token}`);

// Secure WebSocket (if TLS is enabled)
const wss = new WebSocket(`wss://localhost:8443/ws/metrics?token=${token}`);
```

### WebSocket response example

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
        "vramUsage": 18000
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

## Request Examples

### cURL

```bash
# Health check
curl http://localhost:18081/api/v1/health

# Cluster status
curl -X GET http://localhost:18081/api/v1/cluster \
  -H "X-API-Token: your-token"

# List backends
curl -X GET http://localhost:18081/api/v1/backends \
  -H "X-API-Token: your-token"

# Add backend
curl -X POST http://localhost:18081/api/v1/backends \
  -H "Content-Type: application/json" \
  -H "X-API-Token: your-token" \
  -d '{
    "id": "gpu-3",
    "name": "GPU Server 3",
    "host": "192.0.2.80",
    "ollamaPort": 11434,
    "agentPort": 18032,
    "weight": 1,
    "maxConcurrentRequests": 10
  }'

# Delete backend
curl -X DELETE http://localhost:18081/api/v1/backends/gpu-1 \
  -H "X-API-Token: your-token"

# Generate new token (requires master token)
curl -X POST http://localhost:18081/api/v1/auth/token \
  -H "X-API-Token: your-master-token"

# Revoke token
curl -X DELETE "http://localhost:18081/api/v1/auth/token?token=token-to-revoke" \
  -H "X-API-Token: your-master-token"
```

### Python

```python
import requests

# Set up a session with the token
session = requests.Session()
session.headers.update({'X-API-Token': 'your-api-token'})

# Health check (no token)
response = requests.get('http://localhost:18081/api/v1/health')
print(response.json())

# Cluster status
response = session.get('http://localhost:18081/api/v1/cluster')
print(response.json())

# List backends
response = session.get('http://localhost:18081/api/v1/backends')
print(response.json())

# Add backend
new_backend = {
    'id': 'gpu-3',
    'name': 'GPU Server 3',
    'host': '192.0.2.80',
    'ollamaPort': 11434,
    'agentPort': 18032,
    'weight': 1,
    'maxConcurrentRequests': 10
}
response = session.post('http://localhost:18081/api/v1/backends', json=new_backend)
print(response.json())

# Delete backend
response = session.delete('http://localhost:18081/api/v1/backends/gpu-1')
print(response.json())

# Generate token
response = session.post('http://localhost:18081/api/v1/auth/token')
print(response.json())
```

### JavaScript/Node.js

```javascript
const axios = require('axios');

const API_BASE = 'http://localhost:18081';
const TOKEN = 'your-api-token';

const api = axios.create({
  baseURL: API_BASE,
  headers: {
    'X-API-Token': TOKEN
  }
});

// Health check
async function healthCheck() {
  const response = await axios.get(`${API_BASE}/api/v1/health`);
  console.log(response.data);
}

// Cluster status
async function getClusterStatus() {
  const response = await api.get('/api/v1/cluster');
  console.log(response.data);
}

// List backends
async function listBackends() {
  const response = await api.get('/api/v1/backends');
  console.log(response.data);
}

// Add backend
async function addBackend(backend) {
  const response = await api.post('/api/v1/backends', backend);
  console.log(response.data);
}

// Delete backend
async function deleteBackend(id) {
  const response = await api.delete(`/api/v1/backends/${id}`);
  console.log(response.data);
}

// WebSocket connection (token required in query parameter)
const WebSocket = require('ws');
const TOKEN = 'your-api-token';
const ws = new WebSocket(`ws://${API_BASE}/ws/metrics?token=${TOKEN}`);

ws.on('message', (data) => {
  const metrics = JSON.parse(data);
  console.log('Received metrics:', metrics);
});
```

---

## OpenAPI Specification

The full OpenAPI 3.0.3 specification is available in the file [`openapi.yaml`](../openapi.yaml).

### Viewing via Swagger UI

```bash
# Run Swagger UI via Docker
docker run -d -p 8080:8080 -e SWAGGER_JSON=/api/swagger.json \
  -v $(pwd)/docs:/api swaggerapi/swagger-ui

# Then open http://localhost:8080
```

### Using Swagger UI

1. Open https://editor.swagger.io
2. Upload the `openapi.yaml` file
3. Browse and test the API

---

## Summary endpoint table

| Method | Endpoint | Description | Authentication |
|-------|----------|----------|----------------|
| `GET` | `/api/v1/health` | Health check | ❌ |
| `GET` | `/api/v1/cluster` | Cluster status | ✅ |
| `GET` | `/api/v1/metrics` | Metrics for all backends | ✅ |
| `GET` | `/api/v1/metrics/{id}` | Metrics for a backend | ✅ |
| `GET` | `/api/v1/backends` | List backends | ✅ |
| `POST` | `/api/v1/backends` | Add a backend | ✅ |
| `GET` | `/api/v1/backends/{id}` | Backend information | ✅ |
| `PUT` | `/api/v1/backends/{id}` | Update a backend | ✅ |
| `POST` | `/api/v1/backends/{id}/reconfigure` | Reconfigure a backend | ✅ |
| `GET` | `/api/v1/backends/{id}/launch-config` | Backend launch config | ✅ |
| `GET` | `/api/v1/backends/{id}/models` | Models on a backend | ✅ |
| `DELETE` | `/api/v1/backends/{id}` | Remove a backend | ✅ |
| `GET` | `/api/v1/sessions` | List sessions | ✅ |
| `DELETE` | `/api/v1/sessions` | Clear sessions | ✅ |
| `DELETE` | `/api/v1/sessions/{id}` | Delete a session | ✅ |
| `GET` | `/api/v1/models` | Running models | ✅ |
| `GET` | `/api/v1/models/operations` | Active model operations | ✅ |
| `POST` | `/api/v1/agents/register` | Register an agent | ✅ |
| `POST` | `/api/v1/agents/metrics` | Submit metrics from an agent | ✅ |
| `POST` | `/api/v1/agents/heartbeat` | Agent heartbeat | ✅ |
| `GET` | `/api/v1/agents/stats` | Agent statistics | ✅ |
| `GET` | `/api/v1/agents/{id}` | Agent information | ✅ |
| `GET` | `/api/v1/auth/status` | Authentication status | ✅ |
| `POST` | `/api/v1/auth/token` | Create a token | ✅ (master) |
| `DELETE` | `/api/v1/auth/token` | Revoke a token | ✅ (master) |
| `GET` | `/api/v1/queue/stats` | Queue statistics | ✅ |
| `GET` | `/api/v1/queue/details` | Queue details (pending + processing) | ✅ |
| `GET` | `/api/v1/queue/history` | History of completed requests | ✅ |
| `GET` | `/api/v1/predictions` | Predictions for all backends | ✅ |
| `GET` | `/api/v1/predictions/{id}` | Prediction for a backend | ✅ |
| `GET` | `/api/v1/models/capacity` | Global model capacity | ✅ |
| `GET` | `/api/v1/backends/{id}/capacity` | Backend capacity | ✅ |
| `PUT` | `/api/v1/backends/{id}/limits` | Update backend runtime limits | ✅ |
| `GET` | `/api/v1/cluster/config` | Current cluster configuration | ✅ |
| `PUT` | `/api/v1/cluster/config` | Change cluster algorithm/settings | ✅ |
| `POST` | `/api/v1/admin/restart` | Restart the balancer | ✅ |
| `GET` | `/api/v1/proxy/logs` | Proxied-request logs | ✅ |
| `GET` | `/api/v1/candidates` | Candidate groups by model | ✅ |
| `GET` | `/monitor` | Monitor HTML page (canonical `webui/monitor.html`; the `/js`, `/css`, `/img` statics are served by the same server). Data requires a token: `?token=<API_TOKEN>`. The main path for the operator is WebUI `/monitor.html` | ✅ |
| `GET` | `/api/v1/ratelimit/status` | Rate limiter status | ❌ |
| `GET` | `/api/v1/replication/groups` | List replication groups | ✅ |
| `POST` | `/api/v1/replication/groups` | Create a replication group | ✅ |
| `GET` | `/api/v1/replication/groups/{modelName}` | Replication group details | ✅ |
| `PUT` | `/api/v1/replication/groups/{modelName}` | Update a replication group | ✅ |
| `DELETE` | `/api/v1/replication/groups/{modelName}` | Delete a replication group | ✅ |
| `GET` | `/api/v1/replication/stats` | Replication statistics | ✅ |
| `POST` | `/api/v1/replication/reconcile` | Force reconcile | ✅ |
| `GET` | `/api/v1/virtualmodels` | List virtual models | ✅ |
| `GET` | `/api/v1/virtualmodels/{name}` | Virtual model status | ✅ |
| `GET` | `/api/v1/autopull` | Autopull configuration | ✅ |
| `PUT` | `/api/v1/autopull` | Update autopull configuration | ✅ |
| `GET` | `/api/v1/autopull/status` | Autopull status | ✅ |
| `GET` | `/ws/metrics?token=xxx` | Metrics WebSocket | ✅ (token in query) |

---

## Per-Backend API Style (R50 + R51.2)

**Concept:** When a backend is added to `config.json` (or via `/api/v1/backends` POST), the user chooses which API style the backend will use to talk to the balancer:

```json
{
  "id": "cppworker-gpu-bundled",
  "type": "llama_cpp",              // engine type
  "apiStyle": "ollama-native",      // ← R51.2 explicit (JSON tag camelCase), R50 inferred from type
  "host": "cppworker-gpu",
  "cppWorkerPort": 18092
}
```

**Allowed `apiStyle` values:**

| Value | Backend talks via | Client can connect via | Translation |
|----------|------------------------|----------------------------------|-------------|
| `ollama-native` | `/api/*` (Ollama API) | `/api/*` (Ollama) — passthrough | No |
| `ollama-native` | `/api/*` (Ollama API) | `/v1/*` (OpenAI) — request/response | YES — OpenAI↔Ollama conversion |
| `openai-compatible` | `/v1/*` (OpenAI) | `/v1/*` (OpenAI) — passthrough | No |
| `openai-compatible` | `/v1/*` (OpenAI) | `/api/*` (Ollama) — request/response | YES — Ollama↔OpenAI conversion |

**Default behavior (R51.2):**

- `apiStyle` not set, `type: "llama_cpp"` → `EffectiveAPIStyle() = openai-compatible` (R50 behavior, routed via `llamacpp_router.go`)
- `apiStyle` not set, `type: "ollama"` → `EffectiveAPIStyle() = ollama-native` (R50 behavior, routed via `ollama_router.go`)
- `apiStyle: "..."` (any valid one) → `EffectiveAPIStyle() = apiStyle` (explicit override)
- `apiStyle` contains an invalid value → HTTP 400 from `/api/v1/backends` POST/PUT. The internal helper `EffectiveAPIStyle()` falls back to inference (for state.json resilience)

**API contract (R51.2):**

- `POST /api/v1/backends` accepts `apiStyle` in the JSON body (camelCase, `omitempty`)
- `PUT /api/v1/backends/{id}` accepts `apiStyle` (an empty string = preserve existing)
- `GET /api/v1/backends` returns both: `apiStyle` (raw, operator-set) and `effectiveApiStyle` (resolved)

**Code locations:**

- Type + constants: `pkg/types/backend_type.go:14-44`
- Method: `(*Backend).EffectiveAPIStyle()` — `pkg/types/backend_type.go:69-87`
- Field: `Backend.ApiStyle` — `pkg/types/backend.go:53-58`
- Handlers: `internal/api/handlers_backends.go` — `addBackend`, `updateBackend`, `listBackends`
- Tests: `pkg/types/backend_type_test.go` — 21 unit test cases
- Example: `config/backends.example.json`

**R51.3+ plan:** migrate the routing layer (`proxy_request.go:isLlamaCppBackend`) to `EffectiveAPIStyle()`. Adds the Ollama-client → OpenAI-backend path (R52+).

**Design document:** [docs/archive/superpowers/specs/2026-08-19-balancer-api-routing-design.md](../archive/superpowers/specs/2026-08-19-balancer-api-routing-design.md)

---

## Next steps

- [Troubleshooting](../operations/troubleshooting.md) — Troubleshooting
- [Configuration](../getting-started/configuration.md) — Authentication and TLS configuration
- [Deployment](../getting-started/deployment.md) — Production deployment
