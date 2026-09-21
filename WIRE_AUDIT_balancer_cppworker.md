# Wire-Path Audit — balancer ⇄ cppworker

**Scope:** `balancer → cppworker` HTTP contract (method/path/body/headers), cppworker's own HTTP + inference
implementation, token/payload fidelity in both directions, streaming integrity, size limits, tool_calls,
reasoning separation, error propagation.

**Method:** read/grep/glob only. No files modified, no servers started.
Commit state as found on disk.

**Verdict in one line:** the wire path works, but there are **4 critical** defects that silently corrupt data
(one of which makes *every* non-default-setup stream emit the wrong set of `options.*`, one that loses whole
content tokens, three that make the balancer's own metrics cache read zeroes), plus a large tail of
field-level mismatches. The single most important structural fact is that **the balancer never calls the
cppworker's Ollama-native endpoints** — everything goes over `/v1/*` — so the entire Ollama-native cppworker
layer (`/api/chat`, `/api/generate`, `/api/ollama/*`) is dead code on the balancer path.

---

## 0. Exhaustive endpoint map (balancer → cppworker)

Legend: ✅ implemented & wired · ⚠️ implemented but called with wrong/mismatched shape · ❌ called but not implemented · 🚫 implemented but never called by balancer

| # | Caller (balancer file:line) | Method | Path sent to cppworker | Body struct / shape | cppworker handler (file:line) | Status |
|---|---|---|---|---|---|---|
| 1 | `llamacpp_transport.go:41,138` → `/api/chat` | POST | `/v1/chat/completions` | OpenAI Chat (translated) | `handlers_openai.go:216` `handleV1ChatCompletions` | ✅ |
| 2 | `llamacpp_transport.go:41,138` → `/api/generate` | POST | `/v1/completions` | OpenAI Completion (translated) | `handlers_openai.go:1251` `handleV1Completions` | ✅ |
| 3 | `llamacpp_transport.go:41,138` → `/api/embeddings` | POST | `/v1/embeddings` | `{"model","input"}` | `handlers_openai.go:1692` `handleV1Embeddings` | ✅ |
| 4 | `llamacpp_handlers_inference.go:148,364,512` | POST | `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings` | passthrough (post-normalize) | same as above | ✅ |
| 5 | `llamacpp_metrics_poller.go:148` | GET | `/api/models` | – | `handlers_model.go:968` `handleListModels` | ⚠️ **3 fields decode to zero (see F-15)** |
| 6 | `llamacpp_metrics_poller.go:360` | GET | `/api/models/load/progress` | – | `handlers_model.go:823` `handleLoadProgress` | ⚠️ **all duration/size fields decode to zero (F-16)** |
| 7 | `llamacpp_backend_helpers.go:142` | GET | `/api/models` | – | `handlers_model.go:968` | ✅ (only name/path/state used) |
| 8 | `loading_retry.go:215` | GET | `/api/models/load/progress?model=` | – | `handlers_model.go:823` | ✅ |
| 9 | `llamacpp_handlers_readonly.go:135` | GET | `/v1/models` | – | `handlers_openai.go:1786` | ✅ |
| 10 | `llamacpp_handlers_readonly.go:182` | GET | `/api/models/files` | – | `handlers_model.go:1167` | ✅ |
| 11 | `llamacpp_handlers_readonly.go:226` | GET | `/api/tags` | – | `handlers_model.go:1802` `handleOllamaTags` | ✅ |
| 12 | `llamacpp_handlers_readonly.go:383` | GET | `/api/models` | – | `handlers_model.go:968` | ⚠️ **camelCase mirror vs snake_case response (F-15b)** |
| 13 | `llamacpp_handlers_admin.go:26,40,54,68,82,96` | POST | `/api/show`, `/api/create`, `/api/pull`, `/api/delete`, `/api/copy`, `/api/push` | Ollama native, passthrough | `handlers_model.go:1898,1931,66,37` | ✅ |
| 14 | `nctx_reload.go:819` | POST | `/api/models/reload?wait=true&waitTimeoutSec=300` | `reloadModelRequest` | `handlers_model.go:1378` | ✅ (auth required) |
| 15 | `nctx_reload_handlers.go:868` | POST | `/api/models/load` | `loadModelRequest` | `handlers_model.go:26` | ✅ |
| 16 | `model_management.go:779` | POST | `/api/models/load-with-params` | `loadWithParamsRequest` | `handlers_model.go:401` | ✅ |
| 17 | `model_management.go:1055` | POST | `/api/models/unload?name=` | empty | `handlers_model.go:883` | ⚠️ no `force=true` → 409 when busy (F-27) |
| 18 | `nctx_reload_adaptive.go:48` | GET | `/api/v1/cppworker/adaptive/strategy?name=&n_ctx=&gpu_layers=` | – | `adaptive_integration.go:51` → `adaptive_loader.go:881` | ⚠️ **`flashAttnType` never returned (F-23)** |
| 19 | `llamacpp_transport.go:141-149`, `llamacpp_transport_nonstream.go:129-137` | POST | `/api/models/unload?name=` | body `name` moved to query | `handlers_model.go:883` | ✅ |
| 20 | `health.go:198` | GET | `/api/tags` | – | `handlers_model.go:1802` | ✅ |
| 21 | `auto_pull.go:223` | POST | `/api/pull` | Ollama native | `handlers_model.go:66` | ✅ |
| 22 | `llamacpp_runtime_config.go:27` | GET | `/api/v1/cppworker/config/runtime` | – | `handlers_config.go` | ✅ |

**cppworker endpoints implemented but never called by the balancer (🚫):**

| Endpoint | cppworker location | Why it matters |
|---|---|---|
| `/api/chat` | `handlers_chat.go:56` | The whole Ollama-native chat path, including `writeChatStreamResponse` (`handlers_chat.go:573`) and `writeChatStreamResponseWithTools` (`handlers_chat.go:799`). Never reached from the balancer (balancer maps `/api/chat` → `/v1/chat/completions`). **All `eval_count: 0` / `done_reason:"stop"`-hardcoding found in these functions is unreachable via the balancer.** |
| `/api/generate` | `handlers_generate.go:290` | Same: `writeStreamResponse` (`:408`) is balanced out; balancer uses `/v1/completions`. The *better* duration accounting at `handlers_generate.go:504,600-603` is therefore unused on the balancer path. |
| `/api/ollama/generate` | `router.go:43` | alias, unused |
| `/api/ollama/tags` | `router.go:44` | alias, unused (balancer uses `/api/tags`) |
| `/api/embed` | `router.go:41` → `handlers_embeddings.go:98` | Balancer forwards `/api/embed` verbatim (no translation case), so it *is* reachable, but it is **not** registered in `translatePathForLlamaCpp` (`llamacpp_translate_req.go:11-22`) so it bypasses all response translation. |
| `/api/cancel` | `handlers_cancel.go:26` | Balancer relies solely on TCP close / `resp.Body.Close()` for cancellation; it never calls the explicit cancel API (`grep` in `internal/balancer` → **0 matches**). |
| `/api/models/active-queries` | `handlers_active_queries.go` | Balancer never polls it (0 matches). Per-model live query counts are therefore invisible to the balancer. |
| `/api/infer/active`, `/api/infer/users`, `/api/infer/metrics` | `handlers_*` | 0 matches in balancer. |
| `/api/metrics`, `/metrics` | `handlers_metrics.go` | Balancer's `/metrics` at `proxy.go:670` is its own, not a proxy. |
| `/api/models/load/progress/stream` (SSE) | `handlers_model_sse.go:51` | Balancer polls the non-SSE variant. |
| `/api/gpu`, `/api/model`, `/api/diagnostics*`, `/api/hf/*`, `/api/v1/cppworker/config*`, `/api/v1/cppworker/debug/*`, `/api/v1/cppworker/reset-reload-counter`, `/api/version`, `/api/ps` | `router.go:20-88` | Balancer answers most of these itself (aggregated) rather than proxying. |
| `/load` (alias for `/api/models/load`) | `router.go:27` | Never used. |

**Endpoints called by balancer that cppworker does NOT implement: NONE.**
Every `balancer → cppworker` path resolves to a registered `http.ServeMux` pattern in
`cmd/cppworker/router.go:11-107`. I verified each of the 22 paths above against that file.
So the "unimplemented-but-called = critical" category is **empty** — this is the one clean
result of the audit.

---

## 1. ASCII diagram of the ACTUAL request/response flow

```
 CLIENT                     BALANCER (port 18080)                     CPPWORKER (18092)
 ──────                     ─────────────────────                     ─────────────────

 ┌──────────────┐
 │ OpenWebUI    │  POST /api/chat   {"model","messages","options":{...},"stream":true,"tools":[...]}
 │ (Ollama mode)│──────────────────────►┐
 └──────────────┘                       │
 ┌──────────────┐                       │  llamacpp_router.go:73  Route()
 │ Cline / Roo  │  POST /v1/chat/completions                        │  └─► case "/api/chat" → handleChat()
 │ (OpenAI mode)│──────────────────────►┤      case "/v1/chat/…" → handleOpenAIChatCompletions()
 └──────────────┘                       │
                                        ▼
                     ┌──────────────────────────────────────────────────────┐
                     │ llamacpp_handlers_inference.go:548 handleChat        │
                     │  1. io.ReadAll(r.Body)            ← NO SIZE LIMIT ★  │
                     │  2. json.Unmarshal → model                            │
                     │  3. normalizeOpenAIBody()   (openai_normalize.go:112)│
                     │       ↳ flattens messages[].content arrays,          │
                     │         DROPS image_url parts                        │
                     │  4. findModelOnLlamaCppBackend / selectAnyHealthy    │
                     │  5. runInferencePreflight()   (×2 on OpenAI path!) ★ │
                     │  6. ensureModelLoadedOnBackend()                     │
                     │  7. ApplyCppCtxHeader() → sets header X-Cpp-Ctx ★★★  │
                     │  8. isStreamingFromBody()?                           │
                     └───────────────┬──────────────────────────────────────┘
                                     │
             ┌───────────────────────┴───────────────────────┐
             │ stream=true                                   │ stream=false
             ▼                                               ▼
  proxyRequestLlamaCpp()                     proxyRequestLlamaCppNonStream()
  llamacpp_transport.go:28                   llamacpp_transport_nonstream.go:22
             │                                               │
             │  (a) translatePathForLlamaCpp()               │  (a) stripStreamFlagForPath()
             │      /api/chat      → /v1/chat/completions    │  (b) preflightNCtxReloadIfNeededSync()
             │      /api/generate  → /v1/completions         │  (c) translateOllamaBodyToOpenAI()
             │  (b) isStreamingFromBody()                    │  (d) http.NewRequestWithContext()
             │  (c) translateOllamaBodyToOpenAI() ★★★        │
             │  (d) preflightNCtxReloadIfNeeded()            │
             │  (e) unload: move name → query string         │
             │  (f) copy ALL client headers except           │
             │      content-type/accept/content-length/host  │
             ▼                                               ▼
                        HTTP POST  http://cppworker:18092/v1/...
                        Headers: Content-Type: application/json
                                 Accept: application/json, text/event-stream
                                 X-Cpp-Ctx: <resolved n_ctx>        ★★★ contract
                                 X-Request-ID / X-User-Id / X-API-Token (passthrough)
                                     │
                                     ▼
                     ┌───────────────────────────────────────────────────────┐
                     │ recover → cors → RequestIDMiddleware → logging → mux   │
                     │ (router.go:103-106).  Only /api/models/reload,         │
                     │ /api/v1/cppworker/config/*, /debug/* are auth-wrapped. │
                     └───────────────┬───────────────────────────────────────┘
                                     │
                     /v1/chat/completions → handleV1ChatCompletions()
                     handlers_openai.go:216
                       1. types.DecodeJSONRequest(r.Body, MaxStreamingBodyBytes=4MB, &req)
                            ↳ DisallowUnknownFields  (pkg/types/contract_validation.go:39-73)
                       2. ensureModelLoaded()   (may return 503 loading)
                       3. openAIToChatMessage() → buildChatPrompt()
                            ↳ GGUF chat template via bridge, else naive fallback
                       4. augmentSystemWithTools()  (tools → system prompt, capped 8000 chars)
                       5. params := bridge.DefaultGenerationParams()
                       6. ApplyCppCtxHeaderWithOptions(r, &params, {HasTools})
                            ↳ reads X-Cpp-Ctx  (nctx_clamp.go:96-150)
                       7. ComputeContextWarning() → X-Model-Context-Warning header
                       8. if req.Stream { writeOpenAIChatStream() }   ← OpenAI SSE
                          else          { generateWithRamFallback() → JSON }
                                     │
   ┌─────────────────────────────────┴──────────────────────────────────┐
   │ STREAMING (writeOpenAIChatStream, handlers_openai.go:766)          │
   │  ": prefill_started_at=…\n\n"    (SSE comment, 1st bytes)          │
   │  keepalive goroutine  ": keepalive\n\n" every 15s                  │
   │  per token:  data: {"choices":[{"delta":{"role":"assistant",       │
   │                        "content":"tok"}}]}\n\n                     │
   │              or delta.reasoning_content (reasoning models)         │
   │  final:      data: {"choices":[{"delta":{"role":"assistant",       │
   │                        "content":null},"finish_reason":"stop"}]}   │
   │  usage:      data: {"choices":[],"usage":{prompt_tokens,           │
   │                        completion_tokens,total_tokens}}\n\n        │
   │  end:        data: [DONE]\n\n                                      │
   └──────────────────────────────┬─────────────────────────────────────┘
                                  │
        ┌─────────────────────────┴──────────────────────────┐
        │ BALANCER streaming reader                           │
        │  scanner := bufio.NewScanner(resp.Body)             │
        │  scanner.Buffer(make([]byte,0), 1MB)   ✅ bounded   │
        │  for scanner.Scan():  require prefix "data:"        │
        │     · [DONE]            → streamCompleted, break    │
        │     · /v1/chat/…        → filterOpenAIStreamingLine │ ★ drops whole
        │                          + extractToolCallsFromSSE  │   tokens (F-19)
        │     · /api/chat|generate→ translateOpenAISSEData…   │
        │                          → NDJSON {"model","done",  │
        │                            "message"/"response"}    │
        │  after loop: writeStreamingSSEDone / truncation     │
        └─────────────────────────┬──────────────────────────┘
                                  │
      ┌───────────────────────────┴────────────────────────────┐
      │ NDJSON (Ollama clients)        │  SSE (OpenAI clients) │
      │ Content-Type: application/     │  Content-Type: text/  │
      │   x-ndjson                     │    event-stream       │
      │ {…,"done":false}\n             │  data: {…}\n\n        │
      │ {…,"done":true,"done_reason"}  │  data: [DONE]\n\n     │
      └────────────────────────────────┴───────────────────────┘
```

`★` = critical finding, `★★★` = load-bearing contract element.

**Non-streaming sub-flow:** `handlers_openai.go:584` returns one JSON
`chat.completion` with `usage`. The balancer's `translateOpenAIChatToOllama`
(`llamacpp_translate_resp.go:369`) maps `completion_tokens→eval_count`,
`prompt_tokens→prompt_eval_count`, and **hardcodes `total_duration: 0`** (F-35).
If upstream ever emits SSE on the non-stream path, `llamacpp_transport_nonstream.go:532-691`
re-assembles it with a *second* scanner (also 1MB-bounded) and a *separate*, differently-shaped
translation — the two paths disagree on `thinking` vs `reasoning` (F-7/F-8).

**Second SSE reader (hijack path):** `/v1/chat/completions` on the OpenAI handler goes through
`proxyRequestOpenAIStreaming` → `proxyRequestOpenAIStreamingHijacked`
(`proxy_request_hijack.go:77`), which **hijacks the TCP connection** and writes raw chunked HTTP
itself. This path does *not* run any of the balancer's NDJSON translation, does not emit
`X-Model-Context-Warning`, and its own SSE-boundary fix appends `'\n'` to lines that already
end in `'\n'` (`proxy_request_hijack.go:370-372`) — see F-42.

---

## 2. CRITICAL findings

### F-01 · CRITICAL · Balancer's Ollama→OpenAI request translator silently discards 8 of 13 documented Ollama sampling options

**Evidence** — `internal/balancer/llamacpp_translate_req.go:141-157`:

```go
if options, ok := ollamaReq["options"].(map[string]interface{}); ok {
    if temp, ok := options["temperature"]; ok { openaiReq["temperature"] = temp }
    if topP, ok := options["top_p"];  ok { openaiReq["top_p"] = topP }
    if topK, ok := options["top_k"];  ok { openaiReq["top_k"] = topK }
    if maxTokens, ok := options["num_predict"]; ok { openaiReq["max_tokens"] = maxTokens }
    if stop, ok := options["stop"]; ok { openaiReq["stop"] = stop }
}
```

`openaiReq` is built from scratch at `llamacpp_translate_req.go:89-91` — the original Ollama
body is **not** merged in anywhere. Everything not explicitly copied is gone.

**What cppworker is capable of receiving** — `cmd/cppworker/types.go:11-30` (`generateOptions`)
and `handlers_generate.go:34-77,165-186`, which read:

```
Temperature, TopP, TopK, MinP, TypicalP, TfsZ, NumPredict, NumKeep, RepeatPenalty,
FrequencyPenalty, PresencePenalty, RepeatLastN, Mirostat, MirostatTau, MirostatEta,
Seed, NumCtx, Stop
```

and map them into `bridge.GenerationParams` including `RepeatLastN`, `Mirostat`,
`MirostatTau`, `MirostatEta`, `Seed`.

`openAIChatCompletionRequest` (`handlers_openai.go:66-86`) accepts only
`model, messages, max_tokens, temperature, top_p, n, stream, stop, seed, num_ctx,
tools, tool_choice, stream_options`. It has **no** `top_k`, `min_p`, `repeat_penalty`,
`frequency_penalty`, `presence_penalty`, `mirostat*`.

**Concrete failure mode / data lost:** an Ollama client sending

```json
{"model":"x","messages":[…],
 "options":{"num_ctx":32768,"repeat_penalty":1.3,"seed":42,"min_p":0.05,
            "top_k":20,"mirostat":2,"mirostat_tau":5.0,"num_keep":64,
            "typical_p":0.9,"tfs_z":1.1}}
```

reaches cppworker as

```json
{"model":"x","messages":[…],"temperature":…,"top_p":…,"stop":…}
```

Every one of `repeat_penalty`, `seed`, `min_p`, `mirostat`, `mirostat_tau`,
`mirostat_eta`, `num_keep`, `typical_p`, `tfs_z` is **dropped on the balancer side**.
`top_k` *is* forwarded into the OpenAI body but cppworker's strict decoder
(`types.DecodeJSONRequest` → `DisallowUnknownFields`, `contract_validation.go:71`) will
reject the whole request with `400 invalid JSON: unknown field "top_k"` — so `top_k` is
either silently lost or becomes a hard 400 depending on the endpoint reached.

The two user-visible outcomes are therefore:
* non-deterministic output when the user pinned `seed` (reproducibility lost);
* degraded/looping output when the user set `repeat_penalty` (the classic repetition fix
  in Ollama simply doesn't happen);
* hard `400 unknown field "top_k"` for `top_k`.

**Fix:**
1. Add `top_k`, `min_p`, `repeat_penalty`, `repeat_last_n`, `mirostat`, `mirostat_tau`,
   `mirostat_eta`, `typical_p`, `tfs_z`, `num_keep`, `seed`, `frequency_penalty`,
   `presence_penalty` to `openAIChatCompletionRequest` (`handlers_openai.go:66`) and to
   `openAICompletionRequest` (`handlers_openai.go:30`), mapping them into `params` exactly
   as `handlers_generate.go:34-77` already does.
2. Until (1) lands, do **not** forward `top_k` (drop it in the translator) so at least the
   request succeeds — but the correct fix is (1).
3. Add a table-driven test in `internal/balancer/llamacpp_translate_req_test.go` asserting
   round-trip of every `generateOptions` key.

---

### F-02 · CRITICAL · cppworker `/v1/chat/completions` and `/v1/completions` hard-cap the request body at 4 MB — large prompts, images and big tool schemas are rejected with 413 (and the balancer buffers them unbounded first)

**Evidence** — `cmd/cppworker/handlers_openai.go:224` and `:1258`:

```go
if err := types.DecodeJSONRequest(r.Body, types.MaxStreamingBodyBytes, &req); err != nil {
    ...
    case errors.Is(err, types.ErrBodyTooLarge):
        writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
```

and `pkg/types/contract_validation.go:106-108`:

```go
// MaxStreamingBodyBytes is the maximum body size for streaming endpoints
// that may receive large message arrays. 4 MB is enough for ~1K messages.
const MaxStreamingBodyBytes = 4 * 1024 * 1024
```

`cmd/cppworker/handlers_chat.go:65` and `handlers_generate.go:293` use the same 4 MB cap.
`handleV1Embeddings` (`:1702`) gets 32 MB.

**Balancer side has NO cap at all** — `internal/balancer/llamacpp_handlers_inference.go:30`:

```go
bodyBuf, err = io.ReadAll(r.Body)     // unbounded
```

There is **no `http.MaxBytesReader` anywhere on the inference path** — I searched the whole
tree for `http.MaxBytesReader` and found exactly two uses, both in the *admin* profile handler
(`internal/api/handlers_cppworker_profiles.go:153,294`, 64 KB):

```
grep -rn "http\.MaxBytesReader" --include=*.go .
  cmd\cppworker\json_bom_strip_r60_16.go:61:   // oversize). http.MaxBytesReader needs a ResponseWriter for
  internal\api\handlers_cppworker_profiles.go:153:  r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
  internal\api\handlers_cppworker_profiles.go:294:  r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
```

**Concrete failure mode / data lost:**
* 4 MB ≈ 4 M ASCII characters ≈ 1 M English tokens (at Ollama's own ≈4-chars/token
  heuristic, `pkg/.../preflight_nctx.go:75-86`), so a plain 100 K-token text prompt *does*
  fit comfortably. The blow-up comes from non-text payloads and from JSON escaping:
  * Any **base64 image** in an OpenAI `content` array blows past 4 MB instantly (one 3 MB PNG
    → ~4 MB base64, before the rest of the conversation). The balancer itself never enforces a
    limit, so it reads the whole body into RAM, re-marshals it (in `normalizeOpenAIBody`),
    forwards it, and *then* discovers the 413 — a pure amplification path for a trivial DoS
    (N concurrent 1 GB bodies ⇒ N GB resident in the balancer, twice, because
    `bytes.NewReader(translatedBody)` retains a second copy for the retry paths).
  * 20–40 tool schemas at 2–8 KB each is only ~100–300 KB, so tools alone do not breach the
    cap — but they do push a long history with images over it.
* Note the image content is *additionally* discarded by the balancer before it is sent
  (`openai_normalize.go:81-103`, `collectTextParts` drops `image_url` silently *and* emits no
  diagnostic to the client) — so the 413 and the silent drop combine into "vision silently
  doesn't work, and if the image is big you get a 413 instead".

**Fix:**
1. Add `r.Body = http.MaxBytesReader(w, r.Body, limit)` in `handleChat`, `handleGenerate`,
   `handleOpenAIChatCompletions`, `handleOpenAICompletion`, `handleOpenAIEmbeddings`
   (`internal/balancer/llamacpp_handlers_inference.go`), returning a **balancer-shaped** 413
   *before* reading (so the client gets an actionable error and the balancer never buffers).
2. Raise `MaxStreamingBodyBytes` to at least 32 MB (or make it configurable via
   `CPPWORKER_MAX_STREAMING_BODY_BYTES`) and make the two chat/completion handlers use
   `MaxRequestBodyBytes`.
3. Emit an explicit `X-Model-Images-Dropped` header / warning chunk from
   `normalizeOpenAIBody` instead of silently discarding `image_url` parts.

---

### F-03 · CRITICAL · `WriteStreamingSSEDone` emits a **second** `done:true` chunk after a tool-call stream, and never emits `done` at all if the stream ends without `finish_reason`

**Evidence** — `internal/balancer/llamacpp_transport.go:948-969`:

```go
if contentToolCallsProcessed || len(toolAccum) > 0 {
    if !streamCompleted {
        streamCompleted = true
        errFwd := writeStreamingSSEDone(w, originalPath, modelFromCtx, toolAccum,
            accumulatedPlainContent, upstreamDoneContent, false, usageChunkSeen)   // ← writes done:true
        ...
    }
    continue
}
```

This sets `streamCompleted = true` and then `continue`s. The very next upstream chunks —
cppworker's wrapper chunk `{"delta":{"content":null,"role":"assistant"},"finish_reason":"stop"}`
(`handlers_openai.go:1216-1230`) and then the usage chunk
(`emitOpenAIUsageChunkSafe`, `handlers_openai.go:1243`) — fall through to:

```go
if hasFinishReason(data) && !streamCompleted { ... }      // skipped: streamCompleted is true
ollamaChunk := translateOpenAISSEDataToOllamaWithDoneFlag(..., &priorDoneEmitted)
```

`priorDoneEmitted` was **never set to true** on the tool-call branch (it is only set inside the
`if !usageChunkSeen` block at `:1005-1015` for the *normal* branch). So
`translateUsageChunkToOllama` (`llamacpp_translate_resp.go:701-819`) runs and emits a
**second** `{"done":true,"done_reason":…,"prompt_eval_count":…,"eval_count":…}` NDJSON chunk.

Additionally, for the case where cppworker closes the TCP stream *without* `finish_reason`
and *without* `[DONE]` (its `writeOpenAIChatStream` does exactly that when
`generateStreamWithRamFallback` returns an error and the error chunk path is taken —
`handlers_openai.go:1140-1158` writes `finish_reason:"error"` then `data: [DONE]`, but the
`continue` short-circuit above has already flipped `streamCompleted`), the truncation
detection at `llamacpp_transport.go:1165` is **skipped**, so the client gets no terminal
chunk and hangs until TCP timeout.

**Concrete failure mode:** with `tools` present and `stream:true`, an Ollama client sees
`done:true` twice → `ollama` npm / Cline report *"Did not receive done or success response in
stream"* (the exact regression the R53.1 comments at `llamacpp_transport_helpers.go:129-140`
claim to have fixed — the fix is incomplete for the tool-call branch).

**Fix:** in the tool-call branch, set `priorDoneEmitted = true` (and `usageChunkSeen = true`)
immediately after `writeStreamingSSEDone` succeeds, so the subsequent usage chunk is suppressed
by `translateOpenAISSEDataToOllamaWithDoneFlag` (`llamacpp_translate_resp.go:636-638`).
Mirror the guard at `:1005-1015`.

---

### F-04 · CRITICAL · `shouldFilterLlamaCppContent` drops the **entire content token** whenever it merely *contains* a service-token substring — whole words/sentences vanish from OpenAI streams

**Evidence** — `internal/balancer/llamacpp_content_filter.go:129-167`:

```go
func shouldFilterLlamaCppContent(content string) bool {
    trimmed := strings.TrimSpace(content)
    if trimmed == "" { return false }
    lower := strings.ToLower(trimmed)
    filteredTokens := []string{
        "<end_of_turn>", "<start_of_turn>", "</start_of_turn>", "</end_of_turn>",
        "<bos>", "<eos>", "<endoftext>", "<|endoftext|>", "<|eot_id|>", "<|eot|>",
        "<|im_start|>", "<|im_end|>", "<|start_header_id|>", "<|end_header_id|>",
        "<|begin_of_text|>", "<|end_of_text|>", "<sep>", "<pad>", "<unk>",
    }
    for _, tok := range filteredTokens {
        if strings.Contains(lower, tok) {      // ← substring, not equality
            return true
        }
    }
    return false
}
```

and the caller `filterOpenAIStreamingLine` (`:177-238`) which, on `true`, replaces the whole
delta with `{"choices":[{"index":0,"delta":{}}]}` — the token's text is **thrown away**, not
stripped:

```go
filteredChunk := map[string]interface{}{
    "choices": []map[string]interface{}{
        {"index": 0, "delta": map[string]interface{}{}},
    },
}
```

This runs on **every** `/v1/chat/completions` stream, both in `proxyRequestLlamaCpp`
(`:720`) and in the hijack path (`proxy_request_hijack.go:327`).

**Concrete failure mode / data lost:** any token whose text contains one of those substrings
is deleted wholesale. Reachable in ordinary technical conversations:

| model token | contains | result |
|---|---|---|
| `"The <|im_start|> token marks the start of a message"` (may be one vLLM/llama token if BPE merges it) | `<|im_start|>` | whole sentence dropped |
| `"<pad>"` as a quoted example, `<unk>`, `<eos>`, `<sep>`, `<bos>` (all common in ML answers) | exact | dropped |
| `"Use <|end_of_text|> to stop"` | `<|end_of_text|>` | dropped |

The filter is a **substring** match, so `"<sep>"` also matches inside
`"a<sep>arate"`-style garbage, and `<eos>` matches inside any word containing it as a
substring when the token boundary falls that way. The R32 #18 `\\n` → `\n` rewrite in the
cppworker (`handlers_chat.go:646-647`, `handlers_openai.go:1024-1025`) increases the chance
that multi-token text arrives as one token.

**Fix:** in `filterOpenAIStreamingLine`, *strip* the matched tokens from the content and
forward the remainder (only blank the delta if the remainder is empty). Prefer
`strings.EqualFold(strings.TrimSpace(content), tok)` for the "whole chunk is a service token"
case plus `strings.ReplaceAll` for embedded ones — never drop the whole token for a
`Contains` hit.

---

## 3. HIGH findings

### F-05 · HIGH · `head_dim_k` / `head_dim_v` / `n_heads` / `n_kv_heads` / `gpu_layers` / `vram_usage` / `ram_usage` / `total_queries` / `use_mmap` / `flash_attn_type` / `capabilities` / `reasoning_enabled` are dropped by cppworker's `/api/models`, breaking the balancer's own poller

**Evidence** — cppworker builds the per-model entry by hand as a `map[string]interface{}`
(`cmd/cppworker/handlers_model.go:1079-1110`):

```go
entry := map[string]interface{}{
    "name": m.Name, "path": m.Path, "state": m.State,
    "architecture": m.Architecture, "n_layers": m.NLayers, "n_embd": m.NEmbd,
    "n_vocab": m.NVocab, "context_size": m.ContextSize,
    "gguf_context_length": m.GGUFContextLength,
    "size_bytes": effectiveSize, "size": effectiveSize,
    "quantization": parseQuantization(m.Path), "loaded_at": m.LoadedAt,
    "gpu_count": m.GPUCount, "kv_cache_type": m.KVCacheType,
}
if inflight := backend.InFlight(); inflight != nil { entry["active_queries"] = inflight.Get(m.Name) }
```

**Missing** vs `cppbackend.ModelInfo` (`internal/cppbackend/backend.go:65-124`):
`nHeads`, `nKvHeads`, `headDimK`, `headDimV`, `gpuLayers`, `tensorSplit`, `activeQueries`
(mapped only through `InFlight`), `totalQueries`, `batchSize`, `flashAttnType`, `numa`,
`useMmap`, `parallel`, `vram_usage`, `ram_usage`.

The balancer's poller asks for exactly those (`internal/balancer/llamacpp_metrics_poller.go:200-238`):

```go
NLayers   int `json:"n_layers,nLayers,omitempty"`
NHeads    int `json:"n_heads,nHeads,omitempty"`
NKvHeads  int `json:"n_kv_heads,nKvHeads,omitempty"`
HeadDimK  int `json:"head_dim_k,headDimK,omitempty"`
HeadDimV  int `json:"head_dim_v,headDimV,omitempty"`
GPULayers int `json:"gpu_layers,gpuLayers,omitempty"`
TotalQueries int `json:"total_queries,totalQueries,omitempty"`
VRAMUsage uint64 `json:"vram_usage,vramUsage,omitempty"`
RAMUsage  uint64 `json:"ram_usage,ramUsage,omitempty"`
UseMmap   bool   `json:"use_mmap,useMmap,omitempty"`
FlashAttnType int `json:"flash_attn_type,flashAttnType,omitempty"`
Capabilities *types.ModelCapabilities `json:"capabilities,omitempty"`
ReasoningEnabled bool `json:"reasoning_enabled,reasoningEnabled,omitempty"`
```

Note the file's own comment (`llamacpp_metrics_poller.go:179-183`) that a comma-list in a Go
json tag is **not** a fallback — `encoding/json` honours only the first name. So the
snake_case-first tags are the only names accepted, and the response has neither name for the
fields above.

**Concrete failure mode / data lost:** every one of those fields decodes to its zero value in
`types.LlamaCppModel`. Consequences I can name from the code:
* `Capabilities == nil` ⇒ `capabilitiesOrFallback` (`:440-447`) fabricates capabilities from
  the *model name* only → `X-Model-Capabilities` / `X-Model-Max-Context` headers
  (`capabilities_headers.go:19`) can advertise **vision/tools/reasoning the loaded model does
  not have**, and vice versa.
* `VRAMUsage`/`RAMUsage == 0` ⇒ `handlePS` reports `size_vram: 0`
  (`llamacpp_handlers_readonly.go:448`) and the WebUI VRAM/RAM split estimate (which the code
  comments say uses `NLayers`/`NKvHeads`/`NEmbd`/`HeadDimK`/`HeadDimV` — `:284-291`) computes
  nonsense because `HeadDimK/HeadDimV` are 0.
* `GPULayers == 0` ⇒ the "Batched: ON/OFF" and gpu-layers display in `/api/v1/backends`
  always reads 0 → operators cannot see offload state.
* `ContextSize`/`GGUFContextLength` **do** arrive (`context_size`, `gguf_context_length`), so
  the n_ctx preflight itself is not broken — this is a metrics/UI/headers defect, not an
  inference defect.

**Fix:** serialise `ModelInfo` with its own json tags in `handleListModels` instead of
hand-building the map (use `json.Marshal(m)` and augment), or add the missing keys:
`"n_heads"`, `"n_kv_heads"`, `"head_dim_k"`, `"head_dim_v"`, `"gpu_layers"`,
`"total_queries"`, `"vram_usage"`, `"ram_usage"`, `"use_mmap"`, `"flash_attn_type"`,
`"capabilities"`, `"reasoning_enabled"`. Add a contract test mirroring
`cppworker_contract_test.go` for `/api/models`.

### F-06 · HIGH · `/api/models/load/progress` camelCase/snake_case mismatch zeroes every loading progress field

**Evidence** — cppworker emits camelCase (`cmd/cppworker/handlers_model.go:838-845`):

```go
out = append(out, map[string]interface{}{
    "name":             m.Name,
    "state":            m.State,
    "loadingStartedAt": m.LoadingStartedAt.UTC().Format(time.RFC3339Nano),
    "loadingSizeBytes": m.LoadingSizeBytes,
    "elapsedMs":        elapsed,
    "error":            m.LoadingError,
})
```

The balancer poller decodes snake_case (`internal/balancer/llamacpp_metrics_poller.go:384-393`):

```go
LoadingStartedAt string `json:"loading_started_at,loadingStartedAt"`
LoadingSizeBytes int64  `json:"loading_size_bytes,loadingSizeBytes"`
ElapsedMs        int64  `json:"elapsed_ms,elapsedMs"`
```

(The file comment at `:382-383` asserts "LoadingStartedAt/LoadingSizeBytes/ElapsedMs are all
snake_case in cppworker" — that assertion is **false**.)

**Concrete failure mode:** `types.LlamaCppModel.LoadingStartedAt` is `nil`,
`LoadingSizeBytes` and the elapsed counter are `0`, so the WebUI's "Загружается <model> 25s"
progress indicator and the GGUF-tab progress bar render 0 s / 0 bytes for the whole load.
`p.lastLoadingSeen` still flips (it is driven by `len(loading) > 0`, not the parsed fields),
so the 2 s fast-poll does engage — this is a display-fidelity defect only.

**Fix:** change the poller tags to the camelCase names, or (better) change cppworker to
snake_case and make `/api/models` + `/api/models/load/progress` consistent. Add the missing
fields to a shared `pkg/types` struct imported by both sides (the R51.5 contract file's own
"L3" plan, `contract/cppworker_contract.go:22-23`, which was never executed).

### F-07 · HIGH · Reasoning field naming is inconsistent in the wrong direction for Ollama clients: streaming uses `message.thinking`, non-streaming uses `message.reasoning`

**Evidence**

Streaming, `internal/balancer/llamacpp_translate_resp.go:966-984` and `:1044-1048`:

```go
if hasReasoning && !hasContent {
    msg := map[string]interface{}{
        "role":     roleStr,
        "content":  "",
        "thinking": reasoningStr,        // ← "thinking"
    }
```

```go
// Round 29: preserve reasoning_content when emitted in same chunk as content
if hasReasoning { msg["thinking"] = reasoningStr }   // ← "thinking"
```

Non-streaming, same file `:420-425`:

```go
// Round 29 (2026-08-09): reasoning_content → message.reasoning
// (Ollama API). OpenWebUI ожидает message.reasoning.
if rc, ok := message["reasoning_content"].(string); ok && rc != "" {
    msgMap["reasoning"] = rc                           // ← "reasoning"
}
```

A third spelling appears in the SSE-assembled non-stream path,
`llamacpp_transport_nonstream.go:653-655`: `msgMap["reasoning"] = fullReasoning`, and a fourth
in `/api/generate` non-stream: `resp["thinking"] = fullReasoning` (`:643-645`) vs streaming
`/api/generate` `ollamaChunk["thinking"] = reasoningStr`
(`llamacpp_translate_resp.go:1183-1185`).

**Concrete failure mode:** a client that accumulates `message.thinking` during the stream and
then reads the final non-stream/`done` object looking for the same key finds `reasoning`
instead (or vice versa). OpenWebUI's Ollama connector reads `message.thinking`; its
OpenAI connector reads `reasoning_content`; the `/api/chat` non-stream path hands it
`reasoning`. Reasoning text is therefore **not lost**, but which field carries it changes
between modes, and any client that keys on one spelling silently shows an empty think-block.
Additionally the usage/done chunk produced by `translateUsageChunkToOllama`
(`llamacpp_translate_resp.go:805-808`) always sets `message = {"role":"assistant","content":…}`
with **no reasoning/thinking at all**, so the final canonical done chunk for a reasoning
stream never carries the reasoning.

**Fix:** pick Ollama's actual field name (`message.thinking` for `/api/chat`, `thinking` for
`/api/generate`) and use it in *all five* sites. Have `translateUsageChunkToOllama` accept and
emit the accumulated reasoning.

### F-08 · HIGH · `/api/generate` non-stream path emits `thinking`, but the balancer's own translator for the same request emits `reasoning` — plus the `/api/generate` streaming reasoning is routed through the *shared* `chunk` prefix so a reasoning-only chunk can produce an empty `response`

**Evidence** — `llamacpp_translate_resp.go:1183-1198`:

```go
if reasoningStr != "" { ollamaChunk["thinking"] = reasoningStr }
if !hasContent {
    if ollamaChunk["done"] == true { ollamaChunk["response"] = ""; … return }
    if reasoningStr != "" { ollamaChunk["response"] = ""; … return }
    return nil
}
```

That is fine on its own. But `chunk["reasoning_content"]` is read twice with different
sources: first from `choice["reasoning_content"]` (`:1146`) and then again from the **top
level** `chunk["reasoning_content"]` (`:1165`). cppworker only ever emits it inside the choice
(`handlers_openai.go:1517-1519`), so the second read is dead. Meanwhile the *non-stream*
`/api/generate` path (`llamacpp_transport_nonstream.go:643-645`) attaches reasoning to
`resp["thinking"]` **only when `originalPath == "/api/generate"`** and uses `"reasoning"`
inside `msgMap` — consistent with F-07 but inconsistent with the streaming path's `thinking`.

**Fix:** consolidate into one helper `attachReasoning(chunk, path, text)`.

### F-09 · HIGH · Non-stream `/api/embeddings` → `/v1/embeddings` translation turns a valid Ollama request into `{}` and is then re-serialised

**Evidence** — `internal/balancer/llamacpp_translate_req.go:202-216`:

```go
func translateOllamaEmbeddingsToOpenAI(body []byte) ([]byte, error) {
    ...
    openaiReq := map[string]interface{}{ "model": ollamaReq["model"] }
    if input, ok := ollamaReq["input"].(string); ok && input != "" {
        openaiReq["input"] = input
    } else if prompt, ok := ollamaReq["prompt"].(string); ok && prompt != "" {
        openaiReq["input"] = prompt
    }
    return json.Marshal(openaiReq)
}
```

Note `"model"` is read from `ollamaReq`, which produces `"model": null` when the key is
absent — the map entry is always created (`llamacpp_translate_req.go:207-209`). Then
`translatePathForLlamaCpp` maps `/api/embeddings` → `/v1/embeddings`
(`llamacpp_translate_req.go:17-18`) and cppworker's `handleV1Embeddings` requires **both**
`model` and `input` (`handlers_openai.go:1713-1716`):

```go
if req.Model == "" || req.Input == nil {
    writeError(w, http.StatusBadRequest, "model and input are required")
```

**Concrete failure mode:** `"model": null` unmarshals into `req.Model == ""` ⇒ 400
`"model and input are required"`. Any Ollama client calling `/api/embeddings` **without** a
`model` field (some tools omit it when only one model is loaded) now gets a 400 where Ollama
itself would have accepted it. Conversely `{"prompt":"hi","model":"x"}` works.

Also note `translateOllamaBodyToOpenAI` returns `(body, nil)` unchanged on JSON parse failure
(`llamacpp_translate_req.go:86-88`), and `proxyRequestLlamaCpp` falls back to the **raw
untranslated body** on any translation error (`llamacpp_transport.go:44-47`) — so a malformed
Ollama body reaches cppworker's *OpenAI* handler verbatim and produces a confusing
`unknown field` 400 rather than a clear translation error. That silent fallback is itself a
fidelity bug (see F-41).

**Fix:** omit `model` when absent; make the translator return an error the proxy surfaces
instead of falling back to the untranslated body.

### F-10 · HIGH · `options.num_ctx` survives into the OpenAI body for `/api/generate`, but cppworker's strict decoder has no `num_ctx` on `/v1/completions`… wait — it does; the real asymmetry is that the *balancer* strips it on reload while cppworker's `numCtx` spelling differs

**Evidence** — `translateOllamaGenerateToOpenAI` (`llamacpp_translate_req.go:163-198`) copies
**no** `num_ctx` at all: it maps only `temperature`, `top_p`, `max_tokens`, `stop`
(`:183-196`). `translateOllamaChatToOpenAI` likewise copies **no** `num_ctx`
(`:141-157`). So the only channel for `num_ctx` from Ollama clients is the `X-Cpp-Ctx` header
set by `ApplyCppCtxHeader` (`num_ctx_resolver.go:389-398`).

cppworker's OpenAI handlers read the body's `numCtx` (`openAIChatCompletionRequest.NumCtx`,
json tag `num_ctx`, `handlers_openai.go:79`; `openAICompletionRequest.NumCtx`, `:49`) *plus*
the header (`ApplyCppCtxHeaderWithOptions`, `nctx_clamp.go:96-150`) where the **body wins
unless it exceeds the header**:

```go
if params.NCtxOverride <= 0 {
    params.NCtxOverride = headerLimit
} else if params.NCtxOverride > headerLimit {
    params.NCtxOverride = headerLimit     // clamp DOWN only
}
```

The balancer's resolver deliberately clamps *up* to the loaded n_ctx
(`num_ctx_resolver.go:352-370`), then sets the header. So a client asking for `num_ctx: 8192`
against a model loaded at 65536 gets `X-Cpp-Ctx: 65536` and — because the body has no
`num_ctx` — cppworker applies 65536. The **client's explicit small n_ctx is silently ignored**
and the model is forced to the large allocation.

**Concrete failure mode:** a user who sets `num_ctx: 8192` to bound VRAM gets 65536 instead;
on an 8 GB card this is the difference between a working request and an OOM/reload storm.
The balancer's own comment (`num_ctx_resolver.go:24-32`) documents this as intended
("header is an UPPER LIMIT… the balancer policy does NOT raise num_ctx — only limits") but
the code at `:352-370` explicitly *raises* it, contradicting the comment.

**Fix:** emit `num_ctx` into the translated OpenAI body for `/api/chat` and `/api/generate`
(from `options.num_ctx`) so the client's explicit value is carried, and have cppworker treat
the header as a hard ceiling (which `ApplyCppCtxHeaderWithOptions` already does). Remove the
"upgrade to loaded" branch in `ResolveNumCtx` or gate it behind an explicit opt-in.

### F-11 · HIGH · The `X-Model-Context-Warning` signal produced by cppworker is dropped by the balancer on every inference path

**Evidence** — cppworker sets it (`nctx_clamp.go:356-379`, called from
`handlers_chat.go:196`, `handlers_generate.go:349`, `handlers_openai.go:373`) describing
`level | nCtx | usedPercent | nPredict`, and adds it to
`Access-Control-Expose-Headers` (`utils.go:107-110`).

The balancer's inference paths call `w.Header()` themselves and **never copy upstream response
headers**:
* `llamacpp_transport.go:499-507` — sets only `X-Accel-Buffering`, `Cache-Control`,
  `Content-Type`, `Connection`, then `WriteHeader(http.StatusOK)`.
* `llamacpp_transport_nonstream.go:686-689` / `:697-700` — sets only `Content-Type` and
  `Content-Length`.
* `proxy_request_hijack.go:201-224` — writes a **fixed** header block by hand and copies only
  selected upstream headers *excluding* `content-type`/`transfer-encoding`/`content-length`/
  `connection` — but `X-Model-Context-Warning` is not in the exclusion list, so on the hijack
  path it *is* forwarded (raw `fmt.Fprintf`), inconsistently with the other two paths.

**Concrete failure mode:** the client never learns that cppworker silently clamped
`n_predict` (see `handlers_openai.go:365-372`) to fit the context. The user sees a truncated
answer with `finish_reason:"length"` and no explanatory warning, and the `X-Model-Adjusted-
NPredict` header is likewise invisible.

**Fix:** copy `X-Model-Context-Warning`, `X-Model-Context-Suggestion`,
`X-Model-Adjusted-NPredict`, `X-Model-Capabilities`, `X-Model-Max-Context` from `resp.Header`
into `w.Header()` before `WriteHeader` in `proxyRequestLlamaCpp` (both success and 4xx/5xx
paths) and in `proxyRequestLlamaCppNonStream`.

### F-12 · HIGH · Non-stream `/api/chat` and `/api/generate` responses hardcode `total_duration: 0` (and drop the `reasoning` count) while the streaming path computes real durations — the two disagree

**Evidence** — `internal/balancer/llamacpp_translate_resp.go:474-482`:

```go
if usage, ok := openaiResp["usage"].(map[string]interface{}); ok {
    if completionTokens, ok := usage["completion_tokens"].(float64); ok {
        ollamaResp["eval_count"] = int(completionTokens)
    }
    if promptTokens, ok := usage["prompt_tokens"].(float64); ok {
        ollamaResp["prompt_eval_count"] = int(promptTokens)
    }
    ollamaResp["total_duration"] = 0        // ← ALWAYS ZERO, inside the `if usage` block
}
```

Nothing sets `load_duration`, `prompt_eval_duration` or `eval_duration` on this path at all.
The streaming path computes them properly (`llamacpp_translate_resp.go:741-783`,
`translateUsageChunkToOllama`).

**Concrete failure mode:** an Ollama client that uses `stream:false` sees
`total_duration: 0`, `prompt_eval_duration`/`eval_duration` **absent**, so tokens/sec reads
as 0/undefined. Same request with `stream:true` shows real numbers. Monitors that average
`total_duration` across a mixed workload get a bimodal, meaningless series.

**Fix:** measure the request start in `proxyRequestLlamaCppNonStream` (`nonStreamStart`
already exists at `llamacpp_transport_nonstream.go:164`!) and pass it into
`translateOpenAIResponseToOllama` to fill `total_duration`; or read cppworker's
`duration_ms` field, which **is** present in the upstream body
(`handlers_openai.go:597`: `"duration_ms": durationMs`) but is never read by the balancer.
Reading `duration_ms` is the minimal fix.

Related: `llamacpp_transport_nonstream.go:704-720` *does* parse `usage` for
`recordTokenUsage`, so the numbers exist — they are simply not propagated to the client.

---

## 4. MEDIUM findings

### F-13 · MEDIUM · Tool-call `arguments` are concatenated without the `index`-safe merge, and `accumulatedToolCall.index` is never emitted to the client

`internal/balancer/llamacpp_transport_helpers.go:32-102` collects `delta.tool_calls` into
`toolAccum map[int]*accumulatedToolCall` keyed by `index`, concatenating `function.arguments`.
But `writeStreamingSSEDone` (`llamacpp_transport_helpers.go:213-229`) rebuilds the array
**without** `index`:

```go
tcMap := map[string]interface{}{ "type": "function", "function": acc.function }
if acc.id != "" { tcMap["id"] = acc.id }
```

The upstream `index` is dropped. For Ollama `/api/chat` NDJSON this is arguably fine (Ollama's
schema has no `index`), but the *same* helper is used for `originalPath == "/v1/chat/completions"`
— no, it is not: that branch returns `data: [DONE]` immediately (`:154-163`). So the loss is
confined to the Ollama path. **Real defect:** cppworker *does* emit `index`
(`indexedToolCalls`, `handlers_openai.go:1951-1965`) and the OpenAI-spec requires it for
incremental assembly; the balancer's accumulation is therefore only correct if every delta
carries a stable `index`, and if it does not, `idx = len(toolAccum)` (`:67-70`) assigns
colliding indices and **merges two different tool calls into one**.

**Fix:** keep `acc.index` and emit it (harmless for Ollama), and reject/renumber on index
collision rather than silently merging.

### F-14 · MEDIUM · `maxBodySize` for BOM-stripping helper differs from the strict-decoder cap, and the two are used inconsistently

`cmd/cppworker/json_bom_strip_r60_16.go:40` defines
`defaultMaxJSONBodySize = 16 * 1024 * 1024`, used by `decodeJSONRequest(r, &v, 0)`. The strict
path uses `pkg/types.MaxRequestBodyBytes = 32 MB` or `MaxStreamingBodyBytes = 4 MB`. So
which limit applies depends on which helper a handler happens to call:

* `handlers_openai.go:224` (chat) → 4 MB strict
* `handlers_embeddings.go:16,47,109` → 16 MB BOM helper
* `handlers_model.go:1944,2094,2164` → 16 MB
* `handlers_config.go:161` → 16 MB
* `handlers_reset_reload.go:42` → 16 MB

**Fix:** one constant, one helper. Route everything through
`types.DecodeJSONRequest(r.Body, limit, &v)` and delete `decodeJSONRequest`.

### F-15 · MEDIUM · `/api/models` is consumed by **four** different decoders with **four** different field-name sets

* `llamacpp_metrics_poller.go:184-239` — snake_case (`context_size`, `n_layers`, …)
* `llamacpp_backend_helpers.go:177-184` — only `count/name/path/state` ✅
* `llamacpp_types.go:15-32` (`cppWorkerModelsNative`, used by
  `llamacpp_handlers_readonly.go:388-406`) — **camelCase** (`sizeBytes`, `nLayers`, `nHeads`,
  `contextSize`, `gpuLayers`, `activeQueries`, `loadedAt`)
* `llamacpp_handlers_readonly.go:16` `cppWorkerModelState` — only `name/path/state` ✅

cppworker returns **snake_case** from `handleListModels`, so the camelCase mirror at
`llamacpp_types.go:21-30` decodes to all-zero. `fetchLlamaCppModelsNative` is (as far as I can
grep) unused, so this is latent rather than active — but it is a loaded gun: the moment
someone wires it up, every numeric field is 0.

**Fix:** delete `cppWorkerModelsNative` or re-tag it snake_case, and add a single shared
`pkg/types.CppWorkerModelEntry` used by both sides.

### F-16 · MEDIUM · `handlePS` reports `size_vram` in bytes from a MB field and hardcodes empty `digest`/`parameter_size`/zero `expires_at`

`internal/balancer/llamacpp_handlers_readonly.go:433-449`:

```go
details := map[string]interface{}{}
if m.Architecture != "" { details["family"] = m.Architecture }
if m.NLayers > 0 {
    details["parameter_size"] = ""          // ← hardcoded empty
    details["quantization_level"] = m.Quantization
}
allProcesses = append(allProcesses, OllamaProcess{
    Name: m.Name, Model: m.Name,
    Size: int64(m.Size), Digest: "",        // ← always empty
    Details: details, ExpiresAt: time.Time{},
    SizeVRAM: int64(m.VRAMUsage) * 1024 * 1024,
})
```

* `Digest: ""` — Ollama clients (and OpenWebUI's model card) expect `sha256:…`. cppworker
  *does* produce one (`handlers_model.go:1878`: `fmt.Sprintf("sha256:%x", sizeBytes)`), but the
  balancer answers `/api/ps` from its cache and never carries it — and `types.LlamaCppModel`
  has no digest field to carry it in anyway.
* `parameter_size: ""` — the comment at `:437-440` guards on `NLayers > 0` but writes an empty
  string regardless. cppworker computes a real value (`handlers_model.go:1889`).
* `ExpiresAt: time.Time{}` serialises as `"0001-01-01T00:00:00Z"`, which Ollama clients render
  as "expires in 2000 years" or reject.

Since cppworker **already implements** a correct `/api/ps`
(`handlers_model.go:1860-1896`), the cleanest fix is to proxy it (merging across backends)
instead of reconstructing it from a lossy cache.

### F-17 · MEDIUM · cppworker hardcodes `quantization_level: "unknown"` in `/api/tags` and `/api/ps` even though it has the quantisation in the filename

`cmd/cppworker/handlers_model.go:1844` and `:1890`:

```go
"quantization_level": "unknown",
```

while `parseQuantization` (`:1233-1266`) exists and is used for `/api/models`
(`:1091`) and `/api/models/files` (`:1216`). So the same information is present in one
endpoint and thrown away in two others. `handleOllamaShow` also reports `"unknown"`
(`:2001,2023,2065`).

**Concrete failure mode:** the balancer's `fetchLlamaCppTags`
(`llamacpp_handlers_readonly.go:225-300`, fallback path) propagates
`quantization_level: "unknown"` to `/api/tags`, and OpenWebUI's model card shows
"Quantization: unknown" for a model whose file name says `Q4_K_M`.

**Fix:** `"quantization_level": parseQuantization(fm.Filename)` / `parseQuantization(m.Path)`.

### F-18 · MEDIUM · `/api/tags` `digest` is `sha256:<size in decimal>`, not a content hash

`cmd/cppworker/handlers_model.go:1813` and `:1836`:

```go
digest := fmt.Sprintf("sha256:%x", fm.SizeBytes)
```

That is a hex-formatted *byte count*, not a hash. Two different models of identical size get
identical digests. Clients that deduplicate by digest (OpenWebUI does, to key model blobs)
will conflate them.

**Fix:** use a real content hash (or `sha256:` + a stable per-path UUID), or omit the field.

### F-19 · MEDIUM · `tool_calls` produced by cppworker's Gemma parser are truncated at the first `}` (nested arguments lost) — and the same naive regex exists in the balancer's detector

`cmd/cppworker/tool_calls.go:428`:

```go
re := regexp.MustCompile(`(?s)<tool_call>([a-zA-Z_][a-zA-Z0-9_]*)(\{.*?\})</tool_call>`)
```

`\{.*?\}` is lazy, so it stops at the **first** `}`. For
`<tool_call>write_file{"path":"/a","opts":{"mode":"w"}}</tool_call>` it captures
`{"path":"/a","opts":{"mode":"w"}` — unbalanced — and `fixGemmaArgs` (`:482-551`) then
"repairs" it by quote-normalising, producing malformed JSON in `arguments`.

The balancer's mirror has the same shape (`internal/balancer/llamacpp_toolcall_detector.go:235`):

```go
var hermesToolCallRegex = regexp.MustCompile(`(?s)(?:<tool_call>|<answer>)\s*(\{.*?\}+|\[.*?\]+)(?:\s*</tool_call>|\s*</answer>)?`)
```

with `trimTrailingExtraBrace` (`:248`) papering over the missing `}`. Both rely on
`collapseDuplicateClosingBrace` (`:42-52`) which blindly collapses *all* `}}}` → `}}` — that
corrupts legitimately nested `}}}` sequences.

**Fix:** replace the regex with the existing `findMatchingClosingBrace`
(`cmd/cppworker/tool_calls.go:816`; balancer `llamacpp_toolcall_detector.go:576`) which
already tracks depth and string state. Both files already contain that function — the regex
paths simply don't use it.

### F-20 · MEDIUM · `arguments` are passed through as a JSON **object** by the balancer's cross-chunk detector, not as a JSON **string** as OpenAI requires

`internal/balancer/llamacpp_toolcall_detector.go:198-208` (`detectStandardArrayToolCalls`):

```go
var fnMap map[string]interface{}
if err := json.Unmarshal(tc.Function, &fnMap); err != nil { return nil, trimmed, false }
tcMap := map[string]interface{}{
    "id": tc.ID, "type": "function", "function": fnMap,   // fnMap["arguments"] stays an object
}
```

If the model emitted `{"function":{"name":"f","arguments":{"q":"x"}}}` (an object, which is
what most models emit), `fnMap["arguments"]` remains a `map[string]interface{}` and is
re-serialised by `json.Marshal` as a nested object — an OpenAI client doing
`json.loads(tc.function.arguments)` receives a `dict`, not a `str`, and raises
`TypeError: the JSON object must be str, bytes or bytearray`. Note the parser never calls
`stringifyArguments` on this path — cppworker *does* (`tool_calls.go:828-851`), but the
balancer's cross-chunk detector bypasses cppworker entirely for content-embedded tool calls.

**Fix:** coerce `fnMap["arguments"]` to a JSON string before emitting (reuse the
stringify behaviour from `cmd/cppworker/tool_calls.go:828`).

### F-21 · MEDIUM · `memory mapping` / `flash attn` / `kv cache` request preferences are accepted by the balancer's preflight parser and then never sent

`preflight_nctx.go:66-68` parses `RequestedKvCacheType`, `RequestedFlashAttnType`,
`RequestedUseMmap` out of `options.*` and `preflight_nctx.go`'s mismatch logic
(round 34, referenced at `:53-65`) uses them to *decide to reload*. But the reload payload is
built in `nctx_reload.go:785-802` and only ever contains
`name/contextSize/force/gpuLayers/flashAttn/useMmap` — the **client's** requested values are
never forwarded; the adaptive strategy's values win (`nctx_reload_adaptive.go:120-142`).
For a request that does **not** trigger a reload, the client's `flash_attn`/`use_mmap`/
`kv_cache_type` never reach cppworker at all, because the translator
(`llamacpp_translate_req.go:141-157`) does not copy them and cppworker's
`loadWithParamsRequest` is only used for explicit admin loads.

**Fix:** either drop the fields from `RequestMeta` (they mislead), or plumb them into the
reload payload with explicit client-precedence.

### F-22 · MEDIUM · `eval_count` is hardcoded to `0` in cppworker's Ollama-native done chunks (currently unreachable via the balancer, but reachable directly)

`cmd/cppworker/handlers_chat.go:772-781`:

```go
doneChunk := map[string]interface{}{
    "model": …, "created_at": …, "message": …,
    "done": true, "done_reason": "stop",
    "total_duration": duration.Nanoseconds(),
    "eval_count":     0,                        // ← hardcoded
    "eval_duration":  duration.Nanoseconds(),   // ← == total_duration
}
```

and `handlers_chat.go:962-970` (the tools variant) with the identical `eval_count: 0`.
Also `prompt_eval_count` is absent from both.

Anyone hitting cppworker directly on 18092 (the compose file exposes it:
`docker-compose.cppworker-bundled.yml:175-177` `"${CPPWORKER_GPU_PORT:-18092}:18092"`) gets
`eval_count: 0` for every chat response, so tokens/sec is 0.

**Fix:** use the `tokens` counter that already exists on this path
(`llamacpp_transport_nonstream`-style accounting; `writeStreamResponse` in
`handlers_generate.go:433,457` already counts them) and separate
`eval_duration` from `total_duration`.

### F-23 · MEDIUM · `total_duration` and `eval_duration` are the *same* value in cppworker's native `/api/generate` streaming done chunk

`cmd/cppworker/handlers_generate.go:498-508`:

```go
doneChunk := map[string]interface{}{
    …
    "total_duration":    duration.Microseconds() * 1000,
    "eval_count":        tokens,
    "eval_duration":     duration.Microseconds() * 1000,   // ← identical to total_duration
    "tokens_per_second": tps,
}
```

`duration` is measured from `start := time.Now()` at `:435`, i.e. it includes prefill and
model load. `eval_duration` therefore over-reports by the whole prompt-eval time, making
`eval_count / eval_duration` understate tokens/sec. `handlers_generate.go:600-603` does
better (real `loadDuration`, `promptEvalCount`) but that is the `/api/ollama/generate`
handler, unreachable via the balancer.

**Fix:** `eval_duration = total - prompt_eval_duration` (the balancer already does exactly
this split at `llamacpp_translate_resp.go:749-762`).

### F-24 · MEDIUM · `expected_vram`-style accounting from `types.LlamaCppModel.Size` uses the wrong magnitude in `handlePS`

`internal/balancer/llamacpp_handlers_readonly.go:448`:

```go
SizeVRAM: int64(m.VRAMUsage) * 1024 * 1024,
```

`VRAMUsage` is populated from `json:"vram_usage"` (`llamacpp_metrics_poller.go:218`) which
cppworker never sends (F-05) ⇒ always 0. Even if it were sent, the unit is undocumented in
`types.LlamaCppModel`; the multiply-by-1 MiB assumes MB, while cppworker's
`limits.AvailableVRAMMB` etc. are MB. This is a latent unit bug guarded only by the fact that
the source field is always 0.

**Fix:** once F-05 is fixed, document and test the unit; better, have cppworker emit
`vram_usage_bytes`.

### F-25 · MEDIUM · `handleOpenAIChatCompletions` runs the n_ctx preflight **twice** per request, and with an un-normalised body on one of the calls

`internal/balancer/llamacpp_handlers_inference.go:47-49, 81-90, 130-139`:

```go
if normalized := normalizeOpenAIBody(bodyBuf); len(normalized) > 0 { bodyBuf = normalized }
...
if handled, _ := lr.runInferencePreflight(... body: bodyBuf ...); handled { return }   // #1
...
if handled, _ := lr.runInferencePreflight(... body: bodyBuf ...); handled { return }   // #2
```

Both calls pass the same body, so #2 re-evaluates a decision #1 already made. The comments at
`:123-139` explain that #1 was added for a different reason (Round 34) than #2 — the author did
not notice they are the same check. `runInferencePreflight` can *kick off an async reload*
(`nctx_reload_handlers.go` R60.6 dedup relies on `IsReloadPending`, which is only set once the
reload coordinator state is created — between call #1's decision and #2's check there is a
window where the pending flag is not yet visible).

**Fix:** delete the second call (it is strictly redundant), or make #1 the only entry and
assert idempotency.

### F-26 · MEDIUM · `stripStreamFlagForPath` omits cppworker endpoints that do use Ollama streaming, and `isManagementEndpoint` is a hardcoded allow-list that will drift

`internal/balancer/llamacpp_translate_req.go:278-302` lists
`/api/models/load`, `/api/models/load-with-params`, `/api/models/unload`, `/api/models/delete`,
`/api/copy`, `/api/delete`, `/api/pull`, `/api/push`, `/api/create`, `/api/blobs`.
cppworker's `router.go` registers `/load` as an alias for `/api/models/load`
(`router.go:27`) — not in the list. If a client POSTs `/load` with a `stream` field, the
balancer will inject `"stream": false` and cppworker's `handleLoadModel` will reject it with
`400 unknown field "stream"`.

**Fix:** include `/load`, and better: drive the list from a single exported
`pkg/types` variable that cppworker's route table also references.

### F-27 · MEDIUM · Model unload never passes `force=true`, so a busy model silently fails to unload

`internal/balancer/model_management.go:1055`:

```go
url := fmt.Sprintf("http://%s:%d/api/models/unload?name=%s", host, port, safeName)
```

cppworker's `handleUnloadModel` returns **409 Conflict** whenever `active_queries > 0`
(`handlers_model.go:903-925`). The balancer's unload scheduler
(`unload_scheduler.go:253`), model-instance controller (`model_instance_controller.go:304`),
and autotune path (`autotune.go:991-1111`) all call it without `force`. So an idle-unload or an
AutoTune-driven unload of a model under concurrent load is *refused* — and since the caller
does not surface the 409, the operator sees "unload requested" in the logs while VRAM stays
occupied.

**Fix:** pass `force=true` where the balancer genuinely wants to evict (idle unload, autotune,
pre-reload), and *check* the 409 response and log/alert when it is refused.

---

## 5. LOW findings

### F-28 · LOW · `isHeadersSent(w)` is not a reliable "headers already written" test, and after a hijack it is always false

`internal/balancer/llamacpp_handlers_inference.go:799-805`:

```go
func isHeadersSent(w http.ResponseWriter) bool {
    if flusher, ok := w.(http.Flusher); ok { _ = flusher }   // ← no-op
    return w.Header().Get("Content-Type") != ""
}
```

This checks whether the handler happened to set `Content-Type`, not whether `WriteHeader` was
called. Two consequences:
* On the hijack path (`proxy_request_hijack.go:77`) the response headers are written **directly
  to the TCP socket** — `w.Header()` is never touched, so `isHeadersSent` returns `false` after
  a stream has fully started, and `handleChat`/`handleGenerate`/`handleOpenAIChatCompletions`
  then fall into the `writeJSON(w, http.StatusBadGateway, …)` branch
  (`:673`, `:794`, `:168`) → net/http logs `superfluous response.WriteHeader call` and the JSON
  is written into an already-hijacked connection (silently discarded, or interleaved garbage).
* Conversely, any handler that sets `Content-Type` *before* writing and then returns without
  writing gets a bogus SSE error chunk.

**Fix:** wrap the ResponseWriter in a `statusCapturingWriter` (record `WriteHeader` calls) at
the router boundary and expose it, or return an explicit `headersSent bool` from
`proxyRequestLlamaCpp*`.

### F-29 · LOW · `convertCreatedToRFC3339` silently substitutes `time.Now()` on unparseable input

`internal/balancer/llamacpp_translate_resp.go:311-347`: every non-recognised case (unknown
type, unparseable string, bad `json.Number`) falls through to
`return time.Now().UTC().Format(time.RFC3339)`. A model response with `"created": "not-a-date"`
becomes "now" instead of an error or the zero time — masking upstream corruption.

**Fix:** return a sentinel (empty string / `time.Time{}`) and let the caller decide.

### F-30 · LOW · Errors inside a valid SSE chunk are logged at `Errorw` level but the client only receives a generic terminal chunk

`internal/balancer/llamacpp_translate_resp.go:863-876`:

```go
if errStr, ok := chunk["error"].(string); ok && errStr != "" {
    logger.Get().Errorw("translateSSEChatToOllama: upstream SSE chunk with error", …)
    ollamaChunk["done"] = true
    ollamaChunk["done_reason"] = "error"
    ollamaChunk["error"] = errStr
    …
}
```

The `error` string *is* forwarded (good), but cppworker's error chunks put the message in a
nested object for some paths (`{"error": {"message":…, "type":…}}` — e.g.
`llamacpp_transport.go:478` builds exactly that for OpenAI clients) while `translateSSEChatToOllama`
type-asserts `chunk["error"].(string)`. A nested error object is silently ignored and the
client sees a bare `done:true` with no error at all.

**Fix:** handle both `string` and `map[string]interface{}` shapes.

### F-31 · LOW · `intFromUsage` only handles `float64` and `int`, not `json.Number`

`internal/balancer/llamacpp_translate_resp.go:822-830`. If a caller ever configures a
`json.Decoder` with `UseNumber()` upstream of this, every token count silently becomes 0.
Currently `json.Unmarshal` is used so this is latent.

### F-32 · LOW · `handleVersion` on the balancer answers with a local value instead of asking cppworker

`internal/balancer/llamacpp_handlers_readonly.go:457-478`. A version-mismatch between balancer
and the cppworker contract (the exact class of bug the R51.5 contract file was written to
catch) is therefore invisible to `/api/version`. cppworker *does* implement
`/api/version` (`handlers_model.go`, registered at `router.go:51`) and is listed in the
contract registry (`contract/cppworker_contract.go:196`), but the balancer does not use it here.

### F-33 · LOW · `handleModels` merges without dedup and without precedence, unlike `handleTags`

`internal/balancer/llamacpp_handlers_readonly.go:304-385` merges `/api/models` across all
backends; the same model loaded on two backends appears twice (the `handleTags` path dedups by
name, `:114-127`). `count` at `:380` therefore reports the sum, not the unique count.

### F-34 · LOW · `X-Request-ID` (balancer) vs `X-Request-Id` (cppworker / contract) — two different constants

* `internal/balancer/requestid.go:21`: `const requestIDHeader = "X-Request-ID"`
* `pkg/observability/request_id.go:22`: `const HeaderRequestID = "X-Request-Id"`
* `pkg/types/contract_validation.go:213`: `HeaderXRequestID = "X-Request-Id"`

Go's `Header.Set/Get` canonicalise to `X-Request-Id`, so the wire value is identical today.
But the balancer's response echo (`proxy.go:552`) uses the non-canonical constant, and the
contract's own constant differs; any future switch to a non-canonicalising HTTP layer (or a
`Header` map literal) silently breaks end-to-end tracing. `proxy.go:552` and
`requestid.go:21` should use `observability.HeaderRequestID`.

### F-35 · LOW · `writeOpenAIUsageChunk` reports `object: "chat.completion.chunk"` for `/v1/completions`

`cmd/cppworker/handlers_openai.go:1982-2007` hardcodes
`"object": "chat.completion.chunk"`; `writeOpenAICompletionStream` calls it at `:1683` for the
legacy completions endpoint, where the object should be `text_completion`. Strict OpenAI
clients that type-switch on `object` misfile the usage chunk.

### F-36 · LOW · `/v1/models` `created` uses a zero `LoadedAt` for models never loaded

`cmd/cppworker/handlers_openai.go:1793`: `"created": m.LoadedAt.Unix()`. For a disk-known but
unloaded model reached via `/v1/models/` the code special-cases `created == 0`
(`:1847-1850`) but `/v1/models` (`handleV1Models`) does not — it emits
`"created": -62135596800` (year 1). `handleV1ModelByID` handles it; `handleV1Models` does not.

### F-37 · LOW · `types.go` `generateResponse` mixes snake_case and camelCase tags

`cmd/cppworker/types.go:67-80`:

```go
DurationMs       int64   `json:"durationMs"`
TokensPerSec     float64 `json:"tokensPerSec,omitempty"`
TotalDuration    int64   `json:"total_duration,omitempty"`
LoadDuration     int64   `json:"load_duration,omitempty"`
PromptEvalCount  int     `json:"prompt_eval_count,omitempty"`
```

`durationMs`/`tokensPerSec` are camelCase next to snake_case siblings. The balancer never reads
this struct (it goes through `/v1/completions` → `usage`), so this is a consistency defect only.

### F-38 · LOW · `chatRequest` (`handlers_chat.go:34-42`) is a strict-decoded struct that cannot represent what the balancer would send, making the Ollama-native `/api/chat` endpoint structurally unreachable via the balancer

`chatRequest` has only `Model, Messages, Stream, Temperature, MaxTokens, NumCtx, Tools`. With
`DisallowUnknownFields` (`handlers_chat.go:65`) an OpenAI-shaped body containing `top_p`,
`stop`, `tool_choice`, `stream_options`, `seed` gets `400 unknown field`. This is consistent
with the balancer never using it, but it means that if anyone ever flips
`translatePathForLlamaCpp` to map `/api/chat → /api/chat`, requests will break loudly. Document
the constraint or extend the struct.

### F-39 · LOW · Dead code that looks load-bearing: `extractUpstreamGenerateDoneChunk`, `openAILogprobs`-adjacent helpers

* `internal/balancer/llamacpp_transport.go:1355-1382` `extractUpstreamGenerateDoneChunk` — grep
  shows no production caller (the `/api/generate` branch at `:898-932` inlines the logic).
* `cmd/cppworker/tool_calls.go` defines `stripExtraBracesInStringValues`-era helpers that were
  renamed (`fixTrailingBraceMisorder`, `:1173-1202`) with a comment acknowledging the old name
  was misleading.
* `internal/balancer/llamacpp_types.go:15-32` `cppWorkerModelsNative` (F-15).
* `internal/balancer/llamacpp_transport_helpers.go:104-109` `cleanFinalContent` is a one-line
  wrapper around `stripServiceTokens`; both names are used at different call sites.

Dead code on a wire path is a hazard because the *next* reader assumes it is live.

### F-40 · LOW · `emitTruncatedChunk` sets `streamCompleted = true` **before** calling itself, so the truncation detectors downstream are bypassed

`internal/balancer/llamacpp_transport.go:642-670`:

```go
case <-r.Context().Done():
    …
    streamCompleted = true                         // ← set first
    emitTruncatedChunk(w, originalPath, modelFromCtx, time.Since(llamaStartTime))
    return nil
```

Because `streamCompleted` is already true, `handleChat`'s `writeStreamErrorChunk`
(`llamacpp_handlers_inference.go:823`) is not invoked (the function returned `nil`), and the
`if !streamCompleted` block at `:1165` is skipped. The client gets exactly one
`done_reason:"truncated"` chunk — which is the intent — but **no** `error` field and no
`X-Model-*` capability headers, and the `scanner.Err()` that caused the timeout is only logged,
never surfaced. On the OpenAI `/v1/chat/completions` path the emitted chunk carries
`"finish_reason": "truncated"`, a value that is not in the OpenAI spec and that some SDKs
reject.

**Fix:** use a distinct flag (`truncatedEmitted`) rather than overloading `streamCompleted`,
include the real `scanner.Err()` string in the chunk, and map to `finish_reason: "length"` (or
`"stop"` + a top-level `error`) for OpenAI clients.

### F-41 · LOW · A translation failure silently downgrades to forwarding the untranslated body

`internal/balancer/llamacpp_transport.go:44-47`:

```go
translatedBody, err := translateOllamaBodyToOpenAI(originalPath, bodyBuf)
if err != nil {
    translatedBody = bodyBuf          // ← send the Ollama body to an OpenAI endpoint
}
```

and the same at `llamacpp_transport_nonstream.go:119-122`. Because the translators themselves
swallow JSON errors and return the input unchanged (`llamacpp_translate_req.go:86-88`,
`:165-167`, `:204-206`), `err` is in practice never non-nil — so the fallback is unreachable,
which is worse: real translation failures are invisible. A client sending a body the
translator cannot parse sends it verbatim to `/v1/chat/completions` and receives a
`400 unknown field`/`cannot unmarshal` message that names the *OpenAI* struct, which is
impossible to correlate back to the Ollama client's request.

**Fix:** make the translators return a real error on unmarshal failure and surface it as a
`400` with a message that names the client path.

### F-42 · LOW · The hijack path appends an extra `\n` to lines that already end in `\n`, producing `data: {...}\n\n\n`

`internal/balancer/proxy_request_hijack.go:286-288` reads with
`reader.ReadBytes('\n')` (so `line` already ends with `\n` where present), then
`:370-372`:

```go
if isOpenAISSEDataLine(lineToWrite) {
    lineToWrite = append(lineToWrite, '\n')
}
```

For cppworker's `data: {json}\n\n`, the reader yields `"data: {json}\n"` and the code turns it
into `"data: {json}\n\n"` → correct. But for a `data:` line that was *not* newline-terminated
(a final line at EOF, or a chunk that arrived after the reader's buffer split), the append
produces a valid frame followed by a lone `\n` byte on the next read, which is written as a
**1-byte chunked frame** containing just `\n`. The code explicitly skips 1-byte `\n` frames
(`:341-343`) — but only when `lineToWrite` has *not* been modified; the append happens after
that check, so the modified line is never a lone newline. Net effect: benign today, but the
guard and the mutation are in the wrong order. Move the empty-line guard after the append and
re-check.

### F-43 · LOW · `filterOpenAIStreamingLine` drops upstream `usage` blocks and rewrites the chunk from scratch

`internal/balancer/llamacpp_content_filter.go:214-231` rebuilds the chunk from a map that
contains only `choices` plus the *top-level* keys of the original, including `usage`. When the
content check fires on the usage chunk (it has empty choices, so `shouldFilterLlamaCppContent("")`
returns `false` at `:131-133` — so this specific case is safe), fine. But note the rebuilt
chunk **loses `delta.reasoning_content`** for any chunk where content *and* reasoning arrive
together and the content trips the filter — the reasoning half of the token is discarded along
with the content half.

**Fix:** subtract only the matched service tokens; preserve every other delta field.

### F-44 · LOW · `writeStreamErrorChunk` emits `finish_reason: "error"` — not an OpenAI-legal value

`internal/balancer/llamacpp_handlers_inference.go:823-897` uses `"finish_reason": "error"`
for both `/v1/chat/completions` and `/v1/completions`. OpenAI's enum is
`stop|length|tool_calls|content_filter|function_call`. Strict SDKs (openai-python with
pydantic validation) raise on the unknown literal, so the client never processes the error
chunk it was sent to help it.

**Fix:** use `finish_reason: "stop"` plus a top-level `error` object (which the function
already builds), or `"length"` for truncation.

### F-45 · LOW · `sendSSEDone` (balancer, streaming.go:401-422) hardcodes `prompt_eval_count: 0` / `eval_count: 0`

```go
donePayload, _ := json.Marshal(map[string]interface{}{
    "done": true, "total_duration": totalDuration,
    "prompt_eval_count": 0, "eval_count": 0,
})
```

This is the error-path terminal chunk for the *generic* Ollama backend flow
(`handleStreamingResponse`), which is not the cppworker path — but the same helper family is
used when a loop is detected (`streaming.go:272`). Reporting `0/0` for a stream that was
aborted mid-generation makes the client's token accounting wrong rather than absent.

---

## 6. Verified OK

These were specifically searched for and are **absent / correct**:

1. **No `bufio.Scanner` 64 KB silent truncation.** Every scanner on the wire path sets an
   explicit buffer:
   * `internal/balancer/llamacpp_transport.go:576` — `scanner.Buffer(make([]byte, 0), 1024*1024)`
   * `internal/balancer/llamacpp_transport_nonstream.go:539` — same
   * `internal/balancer/proxy_request_openai_auto_stream.go:149` — `make([]byte, 0, 64*1024), 4*1024*1024`
   * `internal/rpccoordinator/worker_client.go:430` — `1024*1024` (RPC path, out of scope)
   I searched the whole tree for `bufio.NewScanner` (357 matches including tests) and every
   production wire-path scanner has a `Buffer` call. **Absent by design.**

2. **`http.MaxBytesReader` is absent from the inference path** — searched the whole tree,
   found only in `internal/api/handlers_cppworker_profiles.go:153,294` (64 KB, admin
   profile CRUD). This is *not* "OK" (it is finding F-02: unbounded `io.ReadAll` in the
   balancer), but the specific claim "is there a MaxBytesReader that could truncate a large
   prompt" resolves to **absent**, so no prompt is silently truncated by a reader limit.
   Truncation risk on this path comes from cppworker's `ErrBodyTooLarge` (4 MB, F-02), not
   from a `LimitReader` cutting a stream.

3. **No `io.LimitReader` on any inference request or response body.** All `io.LimitReader`
   uses are on error/health bodies:
   `internal/cppbackend/hf_downloader.go:314,470,776` (HF API bodies),
   `internal/balancer/auto_continue.go:572` (1 KB error body),
   `internal/balancer/llamacpp_error.go:254` (bounded diagnostic read),
   `pkg/types/contract_validation.go:44` (body-size probe, intentional),
   `cmd/cppworker/json_bom_strip_r60_16.go:64` (`maxBodySize+1` oversize probe, intentional).
   No large prompt is truncated by a limit reader.

4. **`/v1/chat/completions` SSE→SSE passthrough does not buffer, translate, or reorder
   chunks** — each upstream `data:` line is written immediately followed by
   `flusher.Flush()` (`llamacpp_transport.go:723-764`, and the hijack path
   `proxy_request_hijack.go:374-378` writes one chunked frame per line and flushes).
   Chunk boundaries are preserved 1:1.

5. **NDJSON translation flushes after every chunk** — `llamacpp_transport.go:1026-1028`
   (`flusher.Flush()` after each `w.Write(ollamaChunk)`), plus the terminal flush in
   `writeStreamingSSEDone` (`llamacpp_transport_helpers.go:161`).

6. **Chunked terminator is emitted on the hijack path** — `proxy_request_hijack.go:386`
   `writeChunkedFrame(bufrw, nil)` writes `0\r\n\r\n`, and the comment at `:382-385` correctly
   explains why (strict aiohttp parsers). ✅

7. **cppworker flushes after every SSE/NDJSON chunk** — `handlers_openai.go:902,926,2004`,
   `handlers_chat.go:660,674,693`, `handlers_generate.go:456,474`. ✅

8. **The media type check uses Go's canonical header key** — the code queries
   `resp.Header.Get("Content-Type")` (canonical), so the "wrong case → empty string" bug is
   **absent**. `cmd/cppworker/handlers_openai.go:133` does `resp.Header.Get("Content-Type")`
   and `internal/balancer/llamacpp_transport_nonstream.go:531` likewise. ✅

9. **`X-Cpp-Ctx` is present on the primary balancer inference paths** —
   `llamacpp_handlers_inference.go:141,357,649,771` all call `ApplyCppCtxHeader` before
   proxying, and `llamacpp_transport.go:188-196` copies all client headers (including
   `X-Cpp-Ctx`, which `ApplyCppCtxHeader` set on `r.Header`). ✅ **Except** the
   `LB_OPENAI_AUTO_STREAM` path, which builds its own request and drops it — that is
   finding **F-46** in the next section, not an OK.

10. **cppworker's strict decoders do accept every field the balancer actually sends on the
    reload/load/load-with-params paths.** I checked each mirror struct:
    `reloadModelRequest` (`types.go:134-153`) vs the payload built at `nctx_reload.go:785-802`
    + `nctx_reload_adaptive.go:120-159` → `name, contextSize, force, gpuLayers, flashAttn,
    useMmap, kvCacheType, overrideTensors, overrideTensorBufts` — all present. `reason` and
    `adaptiveStage` were correctly removed (comments at `nctx_reload.go:797-801` and
    `nctx_reload_adaptive.go:144-150` document the R51.4 regression). `loadModelRequest`
    (`types.go:89-127`) has `reason` (R44.1). `loadWithParamsRequest` (`types.go:171-210`) is a
    superset. ✅ This is the one part of the contract that is genuinely converged.

11. **`X-Request-Id` propagation is intact** — cppworker's
    `observability.RequestIDMiddleware` is installed inside `recoverMiddleware`/`corsMiddleware`
    (`router.go:103-106`), reads and echoes the header, and the balancer forwards its own
    `X-Request-ID` via the header copy at `llamacpp_transport.go:188-196`. ✅ (modulo F-34's
    constant duplication).

12. **`Ctrl`/`Connection`/`Content-Length`/`Transfer-Encoding` are never blindly copied** —
    `llamacpp_transport.go:190`, `llamacpp_transport_nonstream.go:151`,
    `proxy_request.go:386`, `proxy_request_hijack.go:214-216`,
    `proxy_request_openai_auto_stream.go:118` all exclude them. ✅

13. **Error JSON shape is right for each client family** —
    `writeError` emits `{"error": "<string>"}` (`utils.go:217-219`) which matches
    `ParseCppWorkerError`'s `Error string \`json:"error"\`` (`llamacpp_error.go:39`), and
    `writeCppWorkerErrorWithBridgeInfo` (`utils.go:198-215`) emits the `code` + `bridge_info`
    shape that `isNCtxRelevantCode` / `convertCppWorkerBridgeInfo`
    (`llamacpp_error.go:132-218`) expect, field-for-field
    (`code, current_n_ctx, required_n_ctx, actual_tokens, n_predict, n_ctx_override,
    max_vram_n_ctx, message`). ✅

14. **Multipart/UTF-8 are safe** — bodies are read with `io.ReadAll` and re-marshalled with
    `json.Marshal`, which escapes non-ASCII as `\uXXXX`; no byte-slicing of UTF-8 occurs in
    the translators. The only byte-slicing is `contentStr[1:]` for a leading `\n`
    (`llamacpp_translate_resp.go:1000`, `:1179`) which is safe for a single ASCII newline, and
    `desc[:maxChars]` in `trimDescription` (`tool_calls.go:217`) which **can** split a UTF-8
    rune — a cosmetic defect on a truncated tool description (LOW; noted here for completeness).

---

## 7. Findings added after the "verified OK" pass

### F-46 · HIGH · `LB_OPENAI_AUTO_STREAM=true` drops `X-Cpp-Ctx`, `X-API-Token`, `X-User-Id`, `X-Request-Id` when it rebuilds the upstream request

`internal/balancer/proxy_request_openai_auto_stream.go:78-96`:

```go
upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(upstreamBody))
…
upstreamReq.Header.Set("Content-Type", "application/json")
upstreamReq.Header.Set("Accept", "text/event-stream, application/json")
upstreamReq.ContentLength = int64(len(upstreamBody))
if xff := r.Header.Get("X-Forwarded-For"); xff != "" { upstreamReq.Header.Set("X-Forwarded-For", xff) }
if xri := r.Header.Get("X-Real-IP"); xri != "" { upstreamReq.Header.Set("X-Real-IP", xri) }
```

It does **not** copy `X-Cpp-Ctx` (set moments earlier at
`llamacpp_handlers_inference.go:141`), nor `X-User-Id` (which cppworker uses for per-user
admission control, `handlers_openai.go:251-262`, and for `active_queries` attribution,
`active_generations.go`), nor `X-API-Token` (needed for the auth-wrapped endpoints; harmless
for chat), nor `X-Request-Id` (breaks end-to-end tracing).

**Concrete failure mode:** with `LB_OPENAI_AUTO_STREAM=true`, every non-streaming
`/v1/chat/completions` request loses its n_ctx ceiling → cppworker falls back to the model's
loaded n_ctx and its own defaults instead of the balancer's 3-tier-resolved value →
preflight/reload decisions and the actual inference disagree, and per-user fair-share
(`MaxParallelPerUser`) becomes a single shared bucket keyed by IP.

**Fix:** copy the full `r.Header` with the same exclusion list used in
`llamacpp_transport.go:188-196`, rather than hand-picking two headers.

### F-47 · MEDIUM · `MaxParallelPerUser` is bypassed for every balancer-path request when `X-User-Id` is absent, collapsing all clients into one bucket

`cmd/cppworker/user_id.go:32-33` falls back to `r.RemoteAddr` when `X-User-Id` is missing.
Since the balancer is the TCP peer for all requests and does forward the client's
`X-User-Id` when present (`llamacpp_transport.go:188-196`), any client that does not send it
(the Ollama CLI, plain curl) shares one bucket with every other such client. With
`MaxParallelPerUser=1` that means the second anonymous client is rejected with `429` even
though the first is a different user. Note the balancer derives its own `X-Client-ID` /
session identity (`client.go:186-206`) but does not translate that into `X-User-Id`.

**Fix:** have the balancer set `X-User-Id` from its resolved client identity (session ID or
`X-Client-ID`) when the client did not supply one.

### F-48 · MEDIUM · `intFromUsage` / `buildUsage` compute `completion_tokens` by *re-tokenising the serialised tool-call JSON*, not from the generated token count

`cmd/cppworker/handlers_openai.go:1236-1243`:

```go
if len(toolCalls) > 0 {
    completionText = serializeToolCallsForUsage(indexedToolCalls(toolCalls))
} else {
    completionText = fullOutput
}
emitOpenAIUsageChunkSafe(sw, chatID, created, modelName, prompt, completionText, true)
```

`serializeToolCallsForUsage` (`:686-692`) `json.Marshal`s the `indexedToolCalls` structure —
which contains `index`, `id`, `type`, `function.{name,arguments}` — and
`countTokensSafe(modelName, completionText)` (`:2077-2088`) then runs the model's tokenizer
over that serialisation. The model never generated `"index":0,"type":"function"` framing, so
the reported `completion_tokens` for tool-call responses is inflated by roughly the framing
overhead per call (tens of tokens). `writeToolCallsStream` (`:676`) does the same.

By contrast, `writeOpenAIChatStream` counts real tokens via `sw.IncTokens(1)` per callback
(`:1051,1060`) — and then **ignores** that counter, using `countTokensSafe(fullOutput)` instead
(`:1243`). So the atomic counter is maintained and never used.

**Fix:** use `sw.Tokens()` for the text path (it is the exact generated count) and
`len(calls)`-based accounting plus the real argument-token count for the tool path, or count
tokens of `arguments` only (not the wrapper).

### F-49 · LOW · `buildToolsSystemPrompt` silently truncates the tool set at 8 000 characters and appends `[truncated]`

`cmd/cppworker/tool_calls.go:53,960-968`:

```go
toolsPromptMaxTotalChars = 8000
…
if len(result) > toolsPromptMaxTotalChars {
    result = result[:toolsPromptMaxTotalChars]
    if idx := strings.LastIndex(result, "\n"); idx > toolsPromptMaxTotalChars/2 { result = result[:idx] }
    result += "\n[truncated]"
}
```

With 20+ tools the model is told about only the first few, and nothing is reported to the
client — the model then "fails" to call a tool that exists, and the user sees a plain-text
answer instead of a tool call. `trimDescription` (`:210-223`) also cuts tool descriptions to
80–300 chars.

**Fix:** emit a `X-Model-Tools-Truncated: n/m` header and log at Warn; ideally fail closed with
a 400 when the tool set cannot fit, rather than silently dropping tools.

### F-50 · LOW · `countTokensSafe` returns `len(runes)/4` as if it were an exact count

`cmd/cppworker/handlers_openai.go:2077-2088`: when the model is not loaded or `backend == nil`
the function returns a heuristic. For CJK/Cyrillic text (the project's own comments at
`tool_calls.go` and `utils.go` are largely Russian) `/4` **over**-estimates by ~2×, while for
code/JSON it **under**-estimates. The comment at `:2074-2076` acknowledges this ("Для CJK и
кириллицы это занижает") but the value is then emitted as `usage.prompt_tokens` with no
marker, and the balancer's `recordTokenUsage` (`llamacpp_transport_nonstream.go:716`) stores it
as fact. The balancer's own `EstimatePromptTokens` (`preflight_nctx.go:75-86`) uses the same
heuristic — so a preflight reload decision and the client-visible token count can both be off
by 2× on Cyrillic prompts.

**Fix:** mark heuristic counts (`"usage_estimated": true`), and prefer the model's tokenizer
when loaded (it already is, on this path — `backend.GetModel` fails only for the not-loaded
case).

---

## 8. Severity rollup

| Severity | Count | IDs |
|---|---|---|
| CRITICAL | 4 | F-01, F-02, F-03, F-04 |
| HIGH | 9 | F-05, F-06, F-07, F-08, F-09, F-10, F-11, F-12, F-46 |
| MEDIUM | 18 | F-13 … F-27, F-47, F-48, F-50 |
| LOW | 19 | F-28 … F-45, F-49 |
| **Total** | **50** | |

(The IDs run F-01 … F-50 contiguously. F-46, F-47, F-48 are recorded in §7 because they
surfaced during the "verified OK" sweep; F-50 is filed as LOW in §7.)

**Unimplemented-but-called endpoints: 0.** (Every balancer path resolves in
`cmd/cppworker/router.go`.)

**The four fixes with the largest blast radius, in order:**
1. **F-01** — forward the rest of `options.*` (`repeat_penalty`, `seed`, `min_p`,
   `mirostat*`, `num_keep`, `typical_p`, `tfs_z`, `top_k`); today these are silently dropped
   for every Ollama client.
2. **F-03** — set `priorDoneEmitted`/`usageChunkSeen` on the tool-call terminal branch;
   today tools-over-Ollama-streaming get two `done:true` chunks or none.
3. **F-04** — stop discarding whole content tokens in `shouldFilterLlamaCppContent`; strip
   the token, don't delete the chunk.
4. **F-02** — add `http.MaxBytesReader` on the balancer's five inference handlers and raise
   cppworker's 4 MB streaming cap; today a real multimodal/tool-heavy request is either
   413'd or resident in balancer RAM unbounded.

---

## 9. Files read for this audit

Balancer: `llamacpp_transport.go`, `llamacpp_transport_nonstream.go`,
`llamacpp_transport_helpers.go`, `llamacpp_translate_req.go`, `llamacpp_translate_resp.go`,
`llamacpp_handlers_inference.go`, `llamacpp_handlers_readonly.go`, `llamacpp_handlers_admin.go`,
`llamacpp_metrics_poller.go`, `llamacpp_router.go`, `llamacpp_types.go`, `llamacpp_error.go`,
`llamacpp_content_filter.go`, `llamacpp_toolcall_detector.go`, `llamacpp_backend_helpers.go`,
`nctx_reload.go`, `nctx_reload_handlers.go`, `nctx_reload_adaptive.go`, `num_ctx_resolver.go`,
`preflight_nctx.go`, `openai_normalize.go`, `proxy_request.go`, `proxy_request_hijack.go`,
`proxy_request_openai_auto_stream.go`, `streaming.go`, `stream_flush.go`, `requestid.go`,
`loading_retry.go`, `metrics.go`, `contract/cppworker_contract.go`.

cppworker: `handlers_chat.go`, `handlers_generate.go`, `handlers_openai.go`,
`handlers_model.go`, `handlers_embeddings.go`, `handlers_model_sse.go`, `inference.go`,
`reasoning_content.go`, `tool_calls.go`, `types.go`, `utils.go`, `router.go`, `main.go`,
`nctx_clamp.go`, `json_bom_strip_r60_16.go`, `adaptive_loader.go`, `adaptive_integration.go`,
`safe_stream_writer.go`.

cppbackend: `backend.go`, `batched_scheduler.go`.
Shared: `pkg/types/contract_validation.go`, `pkg/observability/request_id.go`.
Deployment: `deployments/docker-compose.cppworker-bundled.yml`, `deployments/.env.bundled`.
