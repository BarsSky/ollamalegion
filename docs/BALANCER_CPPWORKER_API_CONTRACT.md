# Balancer ↔ cppworker API Contract

**Status:** Authoritative specification for all interaction between `ollama-loadbalancer` and `cppworker`. Implements: Round 36 (Phase 1 of the structural rebuild).

**Audience:** Anyone modifying `cmd/cppworker/**`, `internal/balancer/llamacpp_*`, `internal/balancer/proxy_request*`, or any client code that calls `/api/models/*`, `/v1/chat/completions`, `/api/chat`, `/api/generate` on the cppworker side.

---

## 0. Versioning & Compatibility

### 0.1 Protocol version
The contract is identified by a semantic version `MAJOR.MINOR.PATCH` (e.g. `1.4.0`).
- **MAJOR** bump → breaking change (field removed, status code changed, byte format changed)
- **MINOR** bump → additive (new optional field, new optional header, new endpoint)
- **PATCH** bump → docs only, no code change

### 0.2 Compatibility matrix
- Balancer `N` MUST support cppworker `N-1` and `N`
- cppworker `N` MUST support balancer `N-1` and `N`
- Mismatch is logged at startup but does not block operation (degraded mode)

### 0.3 Version endpoint
- cppworker: `GET /api/version` returns `{"version": "1.4.0", "build": "<commit>"}`
- Balancer: `GET /api/v1/version` returns the same fields plus `{"protocol_version": "1.4.0", "balancer_build": "<commit>", "compatible_with": ["1.3.0", "1.4.0"]}`

### 0.4 Change log
| Version | Date | Change |
|---------|------|--------|
| 1.4.0 | 2026-08-17 | Initial formal contract (Round 36, Phase 1) |
| pre-1.4 | 2026-08-13 | Ad-hoc contract via 35+ rounds of bugfixes |

### 0.5 Per-Backend API Style (R50 + R51.2)

When a backend is registered with the balancer (via `config.json` or `/api/v1/backends` POST), the operator selects the API style the backend will speak:

```json
{
  "id": "cppworker-gpu-bundled",
  "type": "llama_cpp",              // engine type (informational)
  "apiStyle": "ollama-native",      // ← R51.2 explicit field; optional (auto-inferred from type)
  "host": "cppworker-gpu",
  "cppWorkerPort": 18092
}
```

**Allowed `apiStyle` values:**

| Value | Backend speaks | Client may speak | Translation required? |
|-------|----------------|------------------|-----------------------|
| `ollama-native` | `/api/*` (Ollama API) | `/api/*` (Ollama) | No (passthrough) |
| `ollama-native` | `/api/*` (Ollama API) | `/v1/*` (OpenAI-compat) | YES — OpenAI→Ollama on request, reverse on response |
| `openai-compatible` | `/v1/*` (OpenAI API) | `/v1/*` (OpenAI-compat) | No (passthrough) |
| `openai-compatible` | `/v1/*` (OpenAI API) | `/api/*` (Ollama) | YES — Ollama→OpenAI on request, reverse on response |

**R50 (inferred) → R51.2 (explicit field):**

| Field state | R50 behavior | R51.2 behavior |
|-------------|--------------|----------------|
| `apiStyle` not set, `type: "ollama"` | Inferred → `ollama-native` | `Backend.EffectiveAPIStyle()` returns `ollama-native` (same) |
| `apiStyle` not set, `type: "llama_cpp"` | Inferred → `openai-compatible` | `Backend.EffectiveAPIStyle()` returns `openai-compatible` (same) |
| `apiStyle: "ollama-native"` (any type) | NOT supported | Explicit override; used as-is |
| `apiStyle: "openai-compatible"` (any type) | NOT supported | Explicit override; used as-is |
| `apiStyle` set to invalid value | NOT supported | HTTP 400 from `/api/v1/backends` POST/PUT. Internal helper `EffectiveAPIStyle()` falls back to inference (for state.json resilience) |

**Code locations (R51.2):**
- Type + constants: `pkg/types/backend_type.go:14-44` — `APIStyle`, `APIStyleOllamaNative`, `APIStyleOpenAICompatible`, `IsValidAPIStyle()`
- Method: `pkg/types/backend_type.go:69-87` — `(*Backend).EffectiveAPIStyle()`
- Field: `pkg/types/backend.go:53-58` — `Backend.ApiStyle` (JSON tag `apiStyle,omitempty`)
- HTTP handlers: `internal/api/handlers_backends.go` — `addBackend` (~line 320), `updateBackend` (~line 528), `listBackends` (~line 188)
- Tests: `pkg/types/backend_type_test.go` — 21 cases covering constant values, validation, inference priority, JSON round-trip, `omitempty`
- Example: `config/backends.example.json` — both ollama-native and openai-compatible entries

**R51.3+ plan:** migrate routing layer (`proxy_request.go:isLlamaCppBackend`) to choose router by `EffectiveAPIStyle()`, not by `Type`. Adds Ollama-client → OpenAI-backend path (R52+). See [docs/superpowers/specs/2026-08-19-balancer-api-routing-design.md](../superpowers/specs/2026-08-19-balancer-api-routing-design.md) §2 for the model and §8 for the phased migration plan.

**Translation contract:**
- Ollama→OpenAI request conversion: maps `Ollama ChatRequest` fields to `OpenAI ChatRequest` (model, messages, options → messages, max_tokens, temperature, etc.)
- OpenAI→Ollama request conversion: reverse
- Response conversion: maps streaming/non-streaming response format, preserving `finish_reason` semantics

The translation layer is in `internal/balancer/llamacpp_translate_req.go` and `llamacpp_translate_resp.go`. Currently it converts only when client uses `/v1/*` and backend uses Ollama-native; the reverse (Ollama client → OpenAI backend) is not yet implemented but the design supports it.

---

## 1. Streaming byte format (CRITICAL)

This section is **normative**. Any deviation is a bug.

### 1.1 SSE (Server-Sent Events) — used by `/v1/chat/completions` and `/v1/completions`

#### 1.1.1 Response headers (MUST)
```
HTTP/1.1 200 OK
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive
X-Accel-Buffering: no
```

**Forbidden headers:** `Transfer-Encoding: chunked`, `Content-Length: N`. SSE is a long-lived stream; the connection closes when the server decides the response is done. Any `Content-Length` or `Transfer-Encoding: chunked` header that is hand-rolled in hijack paths is wrong (Round 32 #10, #16, #17). Either use Go's `http.ResponseWriter` (which handles this automatically) or omit both.

#### 1.1.2 Event byte sequence (MUST)

Each SSE event is exactly:

```
data: <json>\n
\n
```

The two characters at the end — `\n\n` — are part of the **SSE event boundary** and MUST both be present. The structure is:
- Line 1: `data: <json>` terminated by single `\n`
- Line 2: empty line terminated by single `\n`

**Forbidden sequences:**
- `data: <json>` without trailing `\n\n` (event boundary not closed → client cannot parse the next event)
- Bare `\n` written as a separate event (was the Round 35e bug: `bufio.ReadBytes('\n')` returned the trailing `\n` as a separate `[]byte{0x0A}`; the balancer wrote this as a 1-byte chunked chunk which broke strict aiohttp parsers)
- A single chunked chunk that contains multiple SSE events (each event must be in its own chunked chunk if the underlying transport is chunked)

#### 1.1.3 Termination (MUST)

The final event MUST be exactly `data: [DONE]\n\n` (the literal string `[DONE]` as a payload, not JSON). After `[DONE]`, the server closes the TCP connection.

**Forbidden termination:**
- Closing the connection without `[DONE]` (strict clients raise `TransferEncodingError` or "unexpected EOF")
- Sending a JSON `{"done": true}` event without `[DONE]` (OpenAI-compatible clients expect `[DONE]` terminator)

#### 1.1.4 `finish_reason` requirement (MUST)

The event **immediately before** `[DONE]` MUST contain a `choices[].finish_reason` field that is one of:
- `"stop"` — natural termination
- `"length"` — hit `max_tokens`
- `"tool_calls"` — model invoked a tool
- `"content_filter"` — blocked by safety filter (reserved, not yet used)
- `"cancelled"` — request was cancelled (Round 31 #6: client disconnected, balancer sent abort)

The `finish_reason` MUST NOT be `null`, `""`, or absent in the final non-`[DONE]` event. OpenAI clients depend on this for token accounting.

#### 1.1.5 Event-level invariants (MUST)

For every event between the first and the last (excluding `[DONE]`):
- If `done: false` (NDJSON variant) or no terminator yet (SSE variant): the event MUST have non-empty content OR explicit `finish_reason` OR `tool_calls` array. A "lost token" event (`content: ""` with `done: false` and no `tool_calls` and no `finish_reason`) is a protocol violation. (This was the Round 32 #8 finding from OpenWebUI tests.)
- A `tool_calls` event with `content: ""` is allowed (tool calls are not "content").
- A `reasoning_content` event with `content: ""` is allowed (reasoning is not "content").

#### 1.1.6 Keepalive / heartbeat (SHOULD)

For long streams (>30 seconds without token), the server SHOULD emit a keepalive to prevent client-side timeouts. The accepted formats:
- SSE comment: `: keepalive\n\n` (ignored by all SSE clients, valid per HTML spec § 4.4)
- NDJSON heartbeat: `{"done": false, "_hb": "<rfc3339nano>"}\n` (Round 32 #2 introduced this for `/api/chat`)

**Forbidden keepalive:**
- `data: {"keepalive": true}\n\n` — strict clients try to parse this as a real event
- A bare `\n` without an SSE comment line — was the Round 35e bug

### 1.2 NDJSON — used by `/api/chat` and `/api/generate` (Ollama)

#### 1.2.1 Response headers (MUST)
```
HTTP/1.1 200 OK
Content-Type: application/x-ndjson
Cache-Control: no-cache
Connection: keep-alive
```

**Forbidden:** Same as 1.1.1.

#### 1.2.2 Event byte sequence (MUST)

Each event is exactly one JSON object terminated by a single `\n`:
```
<json>\n
<json>\n
<json>\n
```

**Forbidden sequences:**
- Multiple JSON objects on one line
- A bare `\n` (empty line) without a preceding `{"done": false}` heartbeat
- `\r\n` line terminators (Round 32 #20: balancer was incorrectly emitting `\r\n` in some paths)

#### 1.2.3 Termination (MUST)

The final event MUST have `"done": true` and contain the model's complete response OR an error object. After `{"done": true, ...}`, the server closes the TCP connection.

#### 1.2.4 Per-event schema (NDJSON /api/chat, MUST)

```json
{
  "model": "<model name>",
  "created_at": "<RFC 3339 timestamp>",
  "message": {
    "role": "assistant",
    "content": "<delta text or empty if reasoning or tool_calls>",
    "thinking": "<reasoning delta or empty>",
    "tool_calls": [<array of tool calls or empty>]
  },
  "done": false,
  "done_reason": null
}
```

For the **final** event:
```json
{
  "model": "<model name>",
  "created_at": "<RFC 3339 timestamp>",
  "message": {
    "role": "assistant",
    "content": "<final text or empty>",
    "thinking": "<final reasoning or empty>"
  },
  "done": true,
  "done_reason": "stop" | "length" | "load" | "unload",
  "total_duration": <ns>,
  "load_duration": <ns>,
  "prompt_eval_count": <int>,
  "prompt_eval_duration": <ns>,
  "eval_count": <int>,
  "eval_duration": <ns>
}
```

`done_reason` values:
- `"stop"` — natural termination
- `"length"` — hit `num_predict`
- `"load"` — model had to be loaded (this response is after the load)
- `"unload"` — model was unloaded mid-response (rare; only on client cancel during load)

---

## 2. Async load semantics

### 2.1 Endpoints
- `POST /api/models/load` — primary, supports sync and async via `?wait=` and `?waitTimeoutSec=`
- `POST /api/models/load-with-params` — accepts more parameters, same async semantics
- `POST /api/models/reload` — **DEPRECATED for preflight use** (returns 404 if model not currently loaded). Use `/api/models/load` instead. (Round 35 fix.)
- `GET /api/models/load/progress?model=<name>` — polling endpoint
- `GET /api/models/load/progress/stream?model=<name>` — SSE stream for progress

### 2.2 Default behavior (MUST)

Without any query parameters, `POST /api/models/load` is **async**. It returns HTTP 202 Accepted with a `Location` header pointing to the progress URL. This is the Round 24 default.

### 2.3 Sync mode (?wait=true, SHOULD)

If the client adds `?wait=true&waitTimeoutSec=N`, the server blocks up to `N` seconds for the load to complete, then returns either 200 OK (with the loaded model) or 202 Accepted with `status: "loading_after_timeout"` if the load did not finish in time.

This mode exists for **legacy clients** that do not implement polling (e.g. simple Ollama clients). New clients SHOULD use the default async mode and poll.

### 2.4 Response body for 202 Accepted (MUST)

```json
{
  "status": "loading" | "loading_after_timeout",
  "name": "<model name>",
  "path": "<absolute file path>",
  "loadingSizeBytes": <int>,
  "estimatedLoadTimeMs": <int>,
  "progressUrl": "/api/models/load/progress?model=<urlencoded name>",
  "model": { /* ModelInfo object, may be partial */ },
  "message": "<human-readable>"
}
```

The `Location` header MUST be set to an absolute URL: `http(s)://<host><progressUrl>` or root-relative `<progressUrl>`.

### 2.5 Response body for 200 OK after load (MUST)

```json
{
  "status": "loaded" | "already_loaded" | "dedup",
  "name": "<model name>",
  "path": "<absolute file path>",
  "model": { /* full ModelInfo */ }
}
```

`status: "dedup"` indicates the load was short-circuited because a model with the same `path` was already loaded under a different name. (Round 32 #8 #2 fix.)

### 2.6 Polling contract (MUST)

`GET /api/models/load/progress?model=<name>` returns the **full** list of models with their current state. Clients MUST scan the list for their requested model. Do NOT assume the response is just one model.

Response shape:
```json
{
  "models": [
    {
      "name": "<model name>",
      "state": "unloaded" | "loading" | "loaded" | "unloading" | "error",
      "path": "<absolute file path>",
      "loadedContextSize": <int or null>,
      "loadedGpuLayers": <int or null>,
      "loadedKvCacheType": "f16" | "q8_0" | "q4_0" | null,
      "loadedFlashAttnType": -1 | 0 | 1 | null,
      "loadedUseMmap": true | false | null,
      "error": "<error message>" | null,
      "loadProgress": {
        "elapsedMs": <int>,
        "sizeBytes": <int>,
        "loadHistory": [{"timestampMs": <int>, "sizeBytes": <int>}, ...]
      } | null
    }
  ],
  "count": <int>
}
```

### 2.7 State machine (MUST)

```
unloaded ──POST /load──> loading ──success──> loaded
                            │                    │
                            │                    └─POST /unload──> unloading ──> unloaded
                            │
                            └──failure──> error ──POST /load──> loading
```

`state: "loaded"` is required for the balancer to consider the model ready for inference. `state: "loading"` is transient; the balancer MUST poll until `state` becomes `"loaded"` or `"error"`.

### 2.8 Progress URL semantics (MUST)

The `progressUrl` is a relative URL (root-relative path with query string). The `Location` header in 202 responses MUST be an absolute URL built from `r.Host` and the scheme inferred from `r.TLS`. Clients MAY use either, but absolute URLs are preferred for portability.

---

## 3. Chat / Generate request/response

### 3.1 `/api/chat` (Ollama) — request (MUST)

```json
{
  "model": "<model name>",
  "messages": [
    {"role": "system" | "user" | "assistant" | "tool", "content": "<text>", "images": [<base64>] | null, "tool_calls": [...] | null, "tool_name": "<name>" | null}
  ],
  "stream": true | false,
  "format": "json" | null,
  "options": {
    "num_ctx": <int>,
    "num_predict": <int>,
    "temperature": <float>,
    "top_p": <float>,
    "top_k": <int>,
    "seed": <int>,
    "stop": [<string>],
    "repeat_penalty": <float>,
    "repeat_last_n": <int>,
    "mirostat": <int>,
    "mirostat_eta": <float>,
    "mirostat_tau": <float>,
    "tfs_z": <float>,
    "num_gpu": <int>,
    "main_gpu": <int>,
    "low_vram": true | false,
    "vocab_only": true | false,
    "use_mmap": true | false,
    "use_mlock": true | false,
    "num_thread": <int>
  },
  "keep_alive": "<duration>" | 0 | -1,
  "tools": [<Ollama tool schema>] | null
}
```

Unknown fields MUST be rejected by `json.Decoder.DisallowUnknownFields()` (Round 24 pattern, applied throughout).

### 3.2 `/api/chat` (Ollama) — non-streaming response (MUST)

```json
{
  "model": "<model name>",
  "created_at": "<RFC 3339>",
  "message": {
    "role": "assistant",
    "content": "<final text or empty>",
    "thinking": "<final reasoning or empty>",
    "tool_calls": [<array> | null]
  },
  "done": true,
  "done_reason": "stop" | "length" | "load" | "unload",
  "total_duration": <ns>,
  "load_duration": <ns>,
  "prompt_eval_count": <int>,
  "prompt_eval_duration": <ns>,
  "eval_count": <int>,
  "eval_duration": <ns>
}
```

`message.content` MUST be the concatenation of all streamed `content` deltas. `message.thinking` MUST be the concatenation of all streamed `thinking` deltas. `tool_calls` MUST be the merged final tool calls array.

### 3.3 `/v1/chat/completions` (OpenAI) — request (MUST)

```json
{
  "model": "<model name>",
  "messages": [
    {"role": "system" | "user" | "assistant" | "tool", "content": "<text or array of content parts>", "name": "<name>" | null, "tool_calls": [...] | null, "tool_call_id": "<id>" | null}
  ],
  "stream": true | false,
  "stream_options": {"include_usage": true | false} | null,
  "temperature": <float> | null,
  "top_p": <float> | null,
  "n": 1,
  "max_tokens": <int> | null,
  "max_completion_tokens": <int> | null,
  "stop": <string> | [<string>] | null,
  "presence_penalty": <float> | null,
  "frequency_penalty": <float> | null,
  "seed": <int> | null,
  "logit_bias": <map> | null,
  "logprobs": true | false | null,
  "top_logprobs": <int> | null,
  "user": "<user id>" | null,
  "tools": [<OpenAI tool schema>] | null,
  "tool_choice": "auto" | "none" | "required" | {"type": "function", "function": {"name": "<name>"}} | null,
  "response_format": {"type": "json_object" | "text"} | null,
  "reasoning_effort": "low" | "medium" | "high" | null
}
```

`reasoning_effort` is a non-standard OpenAI extension supported by cppworker. It controls how much reasoning the model produces before the final answer.

### 3.4 `/v1/chat/completions` (OpenAI) — streaming response (MUST)

Each SSE event is `data: <json>\n\n` where the JSON shape is:

```json
{
  "id": "<chatcmpl-XXXX>",
  "object": "chat.completion.chunk",
  "created": <unix epoch seconds>,
  "model": "<model name>",
  "choices": [
    {
      "index": 0,
      "delta": {
        "role": "assistant" | null,
        "content": "<delta or null>",
        "reasoning_content": "<delta or null>",
        "tool_calls": [<delta> | null]
      },
      "finish_reason": null | "stop" | "length" | "tool_calls" | "content_filter" | "cancelled"
    }
  ],
  "usage": null | {"prompt_tokens": <int>, "completion_tokens": <int>, "total_tokens": <int>}
}
```

`object: "chat.completion.chunk"` is **REQUIRED** in every streaming event. The Round 32 #10 bug had balancer emitting `"object": "chat.completion"` (the non-chunk variant) in streaming responses; this is a protocol violation.

The `usage` field is non-null ONLY if `stream_options.include_usage` is true AND this is the **last** event before `[DONE]`. Otherwise `usage` MUST be `null` or absent.

### 3.5 `/v1/chat/completions` (OpenAI) — non-streaming response (MUST)

```json
{
  "id": "<chatcmpl-XXXX>",
  "object": "chat.completion",
  "created": <unix epoch seconds>,
  "model": "<model name>",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "<final text or null if reasoning or tool_calls>",
        "reasoning_content": "<final reasoning or null>",
        "tool_calls": [<array> | null]
      },
      "finish_reason": "stop" | "length" | "tool_calls" | "content_filter" | "cancelled"
    }
  ],
  "usage": {"prompt_tokens": <int>, "completion_tokens": <int>, "total_tokens": <int>}
}
```

`object: "chat.completion"` (NOT `.chunk`) is required for non-streaming responses.

### 3.6 `tool_calls` array format (MUST, both APIs)

Each tool call object:
```json
{
  "id": "<call_XXXX or generated>",
  "type": "function",
  "function": {
    "name": "<function name>",
    "arguments": "<JSON-encoded argument object as a string>"
  }
}
```

`function.arguments` is a **string** containing JSON, not a nested object. This is a common client-side mistake to handle. (Round 32 #8 #1 fix handled gemma-4 emitting arguments without quotes around keys — `{"path": "/foo"}` vs `{path: "/foo"}`; see Section 8.)

### 3.7 `/api/generate` (Ollama) — request and response

Same shape as `/api/chat` but the prompt is a single string under the `prompt` field instead of `messages`. The `response` field replaces `message`.

### 3.8 Unknown field policy (MUST)

All JSON requests MUST be parsed with `json.NewDecoder(r.Body).DisallowUnknownFields()`. This catches client-side bugs where a field name is mistyped (e.g. `max_tokens` vs `maxTokens`). The error response MUST be HTTP 400 with body `{"error": "json: unknown field \"<fieldname>\""}`.

This is currently implemented for `handlers_cppworker_profiles.go:155-163` and MUST be extended to all handlers in `cmd/cppworker/`.

---

## 4. Reasoning content

### 4.1 Field name by API (MUST)

| API | Reasoning field name | Final answer field |
|-----|---------------------|-------------------|
| Ollama `/api/chat` (NDJSON streaming) | `message.thinking` | `message.content` |
| Ollama `/api/chat` (non-streaming) | `message.thinking` | `message.content` |
| Ollama `/api/generate` | `response.thinking` | `response.content` |
| OpenAI `/v1/chat/completions` (streaming delta) | `delta.reasoning_content` | `delta.content` |
| OpenAI `/v1/chat/completions` (non-streaming message) | `message.reasoning_content` | `message.content` |

OpenWebUI uses Ollama paths; Cline/Aider/Continue use OpenAI paths. The balancer MUST translate both directions correctly. (Round 29 fixed this — was the original "OpenWebUI doesn't get reasoning" bug.)

### 4.2 Tag detection in raw model output (cppworker responsibility)

cppworker MUST detect the following reasoning tag pairs in raw model output and route the content between the `thinking` and `content` channels:

| Tag pair | Used by |
|----------|---------|
| `<think>...</think>` | Qwen3, gemma-4 (English reasoning) |
| `<thinking>...</thinking>` | Some custom models |
| `<reasoning>...</reasoning>` | Some custom models |
| `<analysis>...</analysis>` | Some custom models |
| `<\|channel>thought\n...\n<channel\|>` | gemma-4 (chat template canonical) |
| `<\|channel>thought...<channel\|>` | gemma-4 (no trailing newline) |
| `<\|channel>analysis\n...\n<channel\|>` | gemma-4 (analysis variant) |
| `<\|channel>analysis...<channel\|>` | gemma-4 (analysis, no trailing newline) |
| `<\|channel>...<channel\|>` | gemma-4 (bare channel format) |
| `<\|think>...<think\|>` | Qwen-style with pipe delimiters |

The detection order MUST be: longest prefix first (e.g. `<|channel>thought\n` before `<|channel>thought` before bare `<|channel>`). Otherwise the bare pattern would greedily match the prefix of the more specific pattern.

The `stripReasoningTags` post-processing in balancer MUST also handle these same tag pairs (defense in depth).

### 4.3 Leading/trailing whitespace handling (MUST)

If a reasoning block is followed by content, the model may emit a leading `\n` as a separator after `</think>`. The `stripReasoningTags` function in the balancer MUST NOT collapse this `\n` to a space, MUST NOT strip leading newlines from legitimate content, and MUST NOT collapse `\n\n` to `\n` (Round 32 #20 fix).

The previous "defensive" behavior of collapsing whitespace was the root cause of "code blocks rendered as one line" in OpenWebUI. The defensive cleanup is NOT needed because cppworker's `SplitReasoningContent` correctly identifies tag boundaries.

### 4.4 Language-aware thinking instruction (cppworker responsibility)

When `enableThinking: true` is set in the load params, cppworker MUST inject a system-prompt instruction that asks the model to reason. The instruction MUST be in the same primary language as the user's most recent messages. The detection algorithm:

1. Scan the last 4 user/assistant messages
2. Count Cyrillic (U+0400–U+04FF) vs Latin (a-z, A-Z) letters
3. If Cyrillic ≥ 2 × Latin → Russian
4. If Latin > 0 → English
5. Otherwise → universal short instruction (English with "respond in user's language" clause)

The 2× threshold avoids false positives from English technical terms in Russian text (HTML, CSS, JSON, etc.). (Round 32 #12 fix.)

---

## 5. Tool calls

### 5.1 Parsing responsibility (cppworker)

cppworker MUST parse tool calls from the raw model output even if the request body has no `tools: []` array. The "if `len(req.Tools) > 0`" guard is incorrect: many clients (Cline, Aider, Continue) describe tools in the system prompt and do not send the `tools` array. The parser MUST run unconditionally on the model's output. (Round 32 #8 #1 fix.)

### 5.2 Tag wrapping

The model may emit tool calls in any of the following formats:
- `<|tool_call|>{"name": "...", "arguments": {...}}</|tool_call|>` — canonical
- `<tool_call>{"name": "...", "arguments": {...}}</tool_call>` — variant
- `<tool_call>{"name": "...", "arguments": {...}}</tool_call>` — variant (Qwen)
- Bare JSON in the output (Hermes-style)

cppworker MUST handle all variants. Strategy 1b `extractGemmaToolCalls` handles the gemma-4 format where the function name appears BEFORE the JSON object: `<|tool_call|>name(json)</|tool_call|>`.

### 5.3 Argument normalization

gemma-4 may emit relaxed JSON without quotes around keys: `{path: "/foo"}` instead of `{"path": "/foo"}`. cppworker MUST apply a state-machine normalizer (`fixGemmaArgs`) that tracks in-string state and adds quotes around unquoted keys. The normalizer MUST NOT modify content inside string values and MUST NOT modify numeric, boolean, or null values. (Round 32 #8 #1 fix.)

### 5.4 Streaming tool calls

In OpenAI streaming, tool calls are emitted as a series of deltas, each adding one piece of the tool call (id, name, then argument fragments). The balancer MUST merge these deltas into the complete tool call object before forwarding to the client. (Existing balancer logic in `llamacpp_translate_resp.go`.)

In Ollama streaming, the `message.tool_calls` field is `null` for intermediate events and contains the full array on the final event.

---

## 6. Preflight logic (balancer pre-inference check)

### 6.1 Decision matrix

The balancer runs preflight BEFORE forwarding any inference request to cppworker. The decision tree:

```
is_model_loaded_on_backend(model) ?
├── yes → params_match(current_params, request_params) ?
│        ├── yes → NOOP (forward to backend immediately)
│        └── no → RELOAD (background or sync, see 6.3)
└── no → LOAD (background or sync, see 6.2)
```

### 6.2 Model not loaded → LOAD

1. POST `/api/models/load` (async by default)
2. If 202: poll `/api/models/load/progress` until `state: "loaded"` or `state: "error"`
3. If 200: model is already loaded (dedup or already_loaded)
4. If error: return 503 to client with body `{"error": "model load failed: <reason>", "model": "<name>"}`

### 6.3 Model loaded but params mismatch → RELOAD

Reload means "update the loaded model's runtime parameters". The endpoint is `POST /api/models/reload` with the new parameters. If the reload returns 404 (model was unloaded in the meantime), fall back to `POST /api/models/load` (Round 35 fix).

### 6.4 Params to compare (MUST)

The balancer MUST compare these runtime parameters. A mismatch on any of them triggers a reload:

| Parameter | Source | Notes |
|-----------|--------|-------|
| `n_ctx` / `num_ctx` / `contextSize` | profile OR request body | Per-model context size |
| `kv_cache_type` | profile OR request body | `f16` / `q8_0` / `q4_0` |
| `flash_attn` | profile OR request body | `-1` (auto) / `0` (off) / `1` (on) |
| `use_mmap` | profile OR request body | bool |
| `gpu_layers` | profile OR request body | int, `-1` = all, `-2` = auto-fit |

The Round 32 #26 fix (commit `bd9943b`) only compared `n_ctx`. It is now extended to compare all parameters. (Round 36 Phase 2 work.)

### 6.5 Reload vs load endpoint choice

- For explicit admin/UI reload (operator knows current state): use `POST /api/models/reload`
- For preflight (balancer cannot trust current state): use `POST /api/models/load` (idempotent, handles all 3 cases: not loaded, different params, same params)

This avoids the Round 35 bug where preflight called `/api/models/reload` and got 404 when cppworker had been restarted with stale metrics cache.

### 6.6 Async preflight with stream dialog

If the request is streaming AND preflight requires a load, the balancer SHOULD hold the connection open with a stream dialog:
- Hijack the connection
- Send SSE keepalive every 5 seconds
- Wait for the load to complete
- Then proceed with the inference

This avoids `503 + Retry-After` (which the client must implement retry logic for). The Round 32 #26 fix added this for streaming clients.

If the request is non-streaming, return `503 + Retry-After: <seconds>` and let the client retry. (Round 31 #2 fix.)

### 6.7 Polling timeout

The balancer's max wait for the load is computed as:
```
maxWait = min(max(2 × estimatedMs + 60s, cap), clamp)
```
where `estimatedMs` is the `estimatedLoadTimeMs` from cppworker's 202 response, `cap` defaults to 900s (15 min), and the cap is configurable via `LB_NCTX_PREFLIGHT_MAX_WAIT_SEC` (clamp [5, 3600]).

For MoE models with partial offload, the estimation is typically off by 2× — adjust the multiplier accordingly. (Round 35c fix.)

---

## 7. Cancel chain (client → balancer → cppworker → C-bridge)

### 7.1 Propagation order (MUST)

When the client closes the connection (FIN or RST), the cancel MUST propagate in this order, with a max latency of 100ms per hop:

1. **Client → Balancer**: TCP close detected by Go's `net/http`
   - With `ReadTimeout: 5s` (Round 31 #6 fix), the balancer's `r.Context()` is cancelled within 5s
   - With TCP keepalive (Round 31 #6), detection is within 6s on Linux, ~25s on Windows
2. **Balancer → cppworker**: balancer closes the HTTP client connection to cppworker
   - Implemented in `proxy_request_hijack.go` via `polling goroutine` (Round 31 #6)
3. **cppworker → C-bridge**: cppworker's `AbortWatcher` (cmd/cppworker/abort_watcher.go) detects `ctx.Done()` and calls `bridge.RequestAbort(handle)`
   - Latency: 50-200ms
4. **C-bridge → llama_decode**: the atomic abort flag is checked at the top of every `llama_decode` call
   - Latency: 1 llama_decode cycle (~100-500ms for typical batch)

Total: ~6s (Linux) to ~30s (Windows) for the cancel to take effect on the model. This is the hard floor set by the OS; the application code cannot go faster.

### 7.2 Response to client (MUST)

When the cancel propagates all the way:
- Streaming: balancer sends `data: {"choices": [{"finish_reason": "cancelled"}]}\n\ndata: [DONE]\n\n` and closes
- Non-streaming: balancer returns HTTP 200 with `{"choices": [{"finish_reason": "cancelled", "message": {"content": ""}}]}` (the client may interpret this as a normal completion)

The "cancelled" `finish_reason` was added in Round 31 #6 to distinguish true completion from forced cancel. (Round 35e fix made this robust against the empty-line hijack bug.)

### 7.3 Heartbeat for long streams

For streams that may exceed typical client timeouts (reasoning models, 90s+), the cppworker SHOULD emit a keepalive every 5s. See Section 1.1.6. (Round 32 #2 fix.)

### 7.4 Timeout contract

The balancer's defaults for timeouts:

| Timeout | Default | Configurable via | Purpose |
|---------|---------|------------------|---------|
| `LB_REQUEST_TIMEOUT_SEC` | 120 | env | total request timeout |
| `LB_STREAMING_IDLE_TIMEOUT_SEC` | 120 | env | between-chunk timeout (separate from total) |
| `LB_STREAMING_FIRST_BYTE_TIMEOUT_SEC` | 30 | env | wait for first byte from cppworker |
| `LB_NCTX_RELOAD_TIMEOUT_SEC` | 300 | env | time to wait for reload |
| `LB_NCTX_PREFLIGHT_MAX_WAIT_SEC` | 900 | env | max wait for async load |
| `LB_NCTX_PREFLIGHT_WAIT_MULTIPLIER` | 2 | env | multiplier for cppworker estimate |
| `LB_NCTX_PREFLIGHT_WAIT_BUFFER_SEC` | 60 | env | buffer after multiplier |
| `LB_NCTX_PREFLIGHT_ASYNC_RETRY_AFTER_SEC` | 5 | env | 503 Retry-After value |

The two-level timeout (total + idle) is critical: if the cppworker is streaming but stuck (no new tokens for 2 minutes), the idle timeout kicks in. If the cppworker is making slow but steady progress, the total timeout kicks in.

---

## 8. Antiprompts and model-specific behavior

### 8.1 Antiprompt configuration

cppworker maintains a per-model list of antiprompts (tokens that signal the model has finished its response). The default sets:

- **gemma-2** (no reasoning): `<end_of_turn>`, `<start_of_turn>user`, `<start_of_turn>model`
- **gemma-4** (with reasoning): `<start_of_turn>user`, `<start_of_turn>model` — **DO NOT include `<end_of_turn>` because gemma-4 emits this token as part of its reasoning output** (Round 32 #27 fix)
- **Qwen3**: `<|im_end|>`, `<|endoftext|>`
- **Llama-3**: `<|eot_id|>`, `<|end_of_text|>`

### 8.2 Why gemma-4 is different

gemma-4's chat template renders reasoning blocks with the special tokens `<|channel>` and `<|end|>`. The model may emit these tokens as literal text when reasoning about its own output structure (e.g. "the response should end with the `</reasoning>` tag"). If `<end_of_turn>` is in the antiprompt list, the model stops mid-reasoning at the literal token, which appears to the user as "the response was cut off at 85 tokens".

The fix is to only include explicit turn-marker antiprompts (`<start_of_turn>user`, `<start_of_turn>model`) and let `n_predict` or end-of-generation tokens handle the rest.

### 8.3 Detokenization contract

The model output goes through `llama_token_to_piece` with `special=true`. The output MUST be:
- Valid UTF-8
- Newlines (`\n`, U+000A) MUST be real newlines, NOT literal `\\n` (2 chars: backslash + n)
- Tabs (`\t`, U+0009) MUST be real tabs
- Special tokens (e.g. `<|end_of_text|>`) MUST be filtered out (cppworker's responsibility, not the model's)

Round 32 #19 fixed the gemma-4 Q4_K_M detokenization bug where `\\n` (2 chars) was emitted instead of `\n` (1 char). The post-processor in `handlers_chat.go:281-310` replaces any `\\n` (2 chars) with `\n` (1 char) before forwarding. This is a **workaround**; the long-term fix is to update the C-bridge detokenizer to handle the Q4_K_M quantization artifact.

---

## 9. Error model

### 9.1 Error response body (MUST)

All error responses from cppworker (and from balancer endpoints that proxy to cppworker) MUST use this shape:

```json
{
  "error": {
    "message": "<human-readable>",
    "type": "<error class>",
    "code": "<machine-readable code>" | null,
    "param": "<parameter that caused the error>" | null
  }
}
```

For OpenAI-compatible endpoints, the flat form `{"error": "<message>"}` is also accepted for backward compatibility.

### 9.2 HTTP status codes (MUST)

| Code | When |
|------|------|
| 200 | Success (sync) |
| 202 | Accepted (async load, polling required) |
| 400 | Malformed request (invalid JSON, missing required field, unknown field with DisallowUnknownFields) |
| 401 | Missing or invalid auth (X-API-Token mismatch) |
| 403 | Profile disabled, quota exceeded, banned |
| 404 | Model not found, endpoint not found |
| 409 | State conflict (e.g. unload while loading) |
| 413 | Request too large (prompt > n_ctx and cannot be clamped) |
| 429 | Rate limit exceeded (return Retry-After) |
| 500 | Internal cppworker error |
| 502 | Backend (model) error during inference |
| 503 | Service unavailable (model loading, backend unreachable) |
| 504 | Timeout (request exceeded total or first-byte timeout) |

### 9.3 `Retry-After` semantics

- 429 (rate limit): `Retry-After` is the number of seconds the client should wait
- 503 (model loading): `Retry-After` is the estimated time remaining for the load
- 504 (timeout): `Retry-After` is typically omitted; the client should decide

---

## 10. Auth contract

### 10.1 cppworker auth header (MUST)

cppworker accepts either:
- `Authorization: Bearer <token>` (legacy, balancer-registered backends)
- `X-API-Token: <token>` (preferred, matches cppworker's `cfg.Auth.HeaderName`)

cppworker MUST check BOTH headers. (Round 24 fixed this for `nctx_reload.go`; Round 32 #8 should have extended to `handlers_cppworker_profiles.go` but didn't — Round 36 Phase 2 work.)

If `API_TOKEN` env var is set, requests without either header MUST be rejected with 401.

### 10.2 Balancer → cppworker auth propagation

When the balancer proxies a request to cppworker, it MUST forward the auth header from the original request (if present) OR set it from the backend's stored `CppWorkerApiToken` (from registration).

If neither is available, the balancer MUST set `X-API-Token: <backend_stored_token>` (not omit the header). This ensures the round-trip works even if the client didn't send auth.

### 10.3 Cross-tenant isolation

cppworker's `user_id` (from `X-User-Id` header or auth token claim) MUST be used for:
- Per-user parallel limits (`MaxParallelPerUser`)
- Audit log
- Per-user token usage statistics

The balancer MUST NOT mix user_ids when forwarding requests to a shared backend.

---

## 11. State propagation (cross-component)

### 11.1 Load callbacks (cppworker → balancer)

When a model is loaded or unloaded on cppworker, the cppworker SHOULD notify the balancer via the callback endpoints:

- `POST <balancer>/api/v1/internal/llama-model-loaded` with body `{"name": "...", "path": "...", "context_size": N, "gpu_layers": N, "kv_cache_type": "...", "flash_attn": N, "use_mmap": bool}`
- `POST <balancer>/api/v1/internal/llama-model-unloaded` with body `{"name": "..."}`

The balancer MUST update its `lastKnownNCtx` cache and `metrics.Models` list based on these callbacks. This avoids the Round 32 #15 / Round 33 bug where stale metrics cache caused preflight to make wrong decisions after a cppworker restart.

If the callback fails (e.g. balancer unreachable), cppworker MUST log the error but NOT retry indefinitely (one shot is enough — the 30s metrics poller is the fallback).

### 11.2 Metrics poller (balancer → cppworker, fallback)

The balancer runs a periodic metrics poller that calls `GET /api/models` on each backend every 30s. This is the **fallback** to the callback mechanism in 11.1. The poller updates `lastKnownNCtx`, `metrics.Models`, and `metrics.LlamaCpp.LoadedModels`.

Both mechanisms MUST be active; the callbacks reduce the staleness window, the poller guarantees eventual consistency.

---

## 12. Implementation checklist

This contract is enforced by:

- **Phase 2 (Type-safe models)**: Go structs with `DisallowUnknownFields()` in every handler. See `pkg/types/llamacpp_contract.go` (Round 36 Phase 2).
- **Phase 3 (Conformance tests)**: Golden files in `testdata/cppworker_golden/*.json` capture exact byte sequences. See `internal/balancer/contract_conformance_test.go` (Round 36 Phase 3).
- **Phase 4 (Observability)**: X-Request-Id propagation enables end-to-end tracing. See `internal/observability/request_id.go` (Round 36 Phase 4).

Any change to the protocol MUST update:
1. This document
2. The Go structs in Phase 2
3. The golden files in Phase 3
4. The CHANGELOG with a "PROTOCOL: bump to 1.X.Y" entry

---

## 13. References

- Round 32 #1: gemma-4 channel format reasoning tags
- Round 32 #8: bare `<|channel>` pattern + LoadModelWithOpts dedup by file path
- Round 32 #9: stream/idle timeout heuristic bump
- Round 32 #10: drop broken `Transfer-Encoding: chunked` (later reversed)
- Round 32 #11: further timeout bump for long reasoning
- Round 32 #12: language-aware thinking instruction
- Round 32 #13: explicit CODE RULES in thinking instructions
- Round 32 #16: chunked-framed SSE in hijack path
- Round 32 #17: SSE event boundary (`\n\n`) in chunked hijack
- Round 32 #18: fix `\\n` in system prompt
- Round 32 #19: gemma-4 Q4_K_M detokenizer workaround
- Round 32 #20: remove collapse `\n\n` and trim leading `\n` in stripReasoningTags
- Round 32 #26: 4-phase preflight fix
- Round 32 #27: gemma-4 `<end_of_turn>` antiprompt removed
- Round 35: reload→load fallback
- Round 35c: env-tunable async load polling
- Round 35e: empty SSE lines in hijack path
