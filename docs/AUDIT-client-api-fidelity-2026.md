# OllamaLegion — Client-Facing API Compatibility & Data Fidelity Audit

**Scope:** `internal/balancer/*.go`, `internal/api/*.go`, `cmd/balancer/*` — how the balancer serves the Ollama-native and OpenAI-compatible API surface to third-party clients (OpenWebUI, Cline/Roo Code, ollama-python/ollama-js, Continue, Aider, Hermes).

**Method:** direct source reading (no servers started, no files modified). Every finding cites `file:line` with a short quote. `cmd/cppworker/*` and `pkg/types/*` were read only to establish what the *upstream* contract actually accepts/rejects, which is what makes several balancer-side drops observable to clients.

**Read-only note:** this audit did not modify any project file. The report itself is a new file.

---

## 0. Architecture summary (needed to judge the findings)

```
client ──▶ cmd/balancer/main.go  mux.Handle("/", proxy)      (proxy port, default 18080)
                 │
                 └─▶ Proxy.ServeHTTP (proxy.go:540)
                        ├─ /api/v1/*      ─▶ reverse-proxy to API server (router.go:61)
                        ├─ early-404 for unsupported paths (router.go:69)
                        └─ routeRequest (router.go:48)
                              └─ dispatchRouters → [OllamaRouter, LlamaCppRouter] (router.go:106-109)
                                     ├─ OllamaRouter.Route  (ollama_router.go:36)
                                     └─ LlamaCppRouter.Route (llamacpp_router.go:73)
                        └─ otherwise main proxy flow → proxyRequest (proxy_request.go:140)
                              └─ if backend.EffectiveAPIStyle()==openai-compatible:
                                     translateOllamaBodyToOpenAI → cppworker /v1/chat/completions
```

Two things dominate the audit:

1. **`/api/*` requests to a llama.cpp backend are rewritten into OpenAI requests** (`llamacpp_translate_req.go:11-40`), and cppworker's OpenAI handlers use `DisallowUnknownFields` (`pkg/types/contract_validation.go:71`, `cmd/cppworker/handlers_openai.go:224`). Any field the translator *adds* that the upstream struct does not know about causes a hard **HTTP 400**, and any field it *forgets* is silently lost.
2. **The upstream OpenAI structs are much narrower than the Ollama structs.** `openAIChatCompletionRequest` (handlers_openai.go:66-86) accepts only `model, messages, max_tokens, temperature, top_p, n, stream, stop, seed, num_ctx, tools, tool_choice, stream_options`. `generateRequest` (cmd/cppworker/types.go:32-65) accepts the full Ollama option set. So Ollama→OpenAI translation is a lossy funnel.

---

## 1. Findings ordered by severity

### CRITICAL

---

#### C1. `options.top_k` is forwarded as OpenAI `top_k` → upstream rejects with HTTP 400 (breaks every OpenWebUI `/api/chat` with advanced params)

**Evidence**
```go
// internal/balancer/llamacpp_translate_req.go:148-150
if topK, ok := options["top_k"]; ok {
    openaiReq["top_k"] = topK
}
```
Upstream struct (`cmd/cppworker/handlers_openai.go:66-86`) has **no `top_k`**:
```go
type openAIChatCompletionRequest struct {
	Model     string              `json:"model"`
	Messages  []openAIChatMessage `json:"messages"`
	MaxTokens int                 `json:"max_tokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	N           int      `json:"n,omitempty"`
	Stream      bool     `json:"stream,omitempty"`
	Stop        []string `json:"stop,omitempty"`
	Seed        int      `json:"seed,omitempty"`
	NumCtx int `json:"num_ctx,omitempty"`
	Tools []openAITool `json:"tools,omitempty"`
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
	StreamOptions *openAIStreamOptions `json:"stream_options,omitempty"`
}
```
And the decoder is strict:
```go
// cmd/cppworker/handlers_openai.go:221-233
var req openAIChatCompletionRequest
if err := types.DecodeJSONRequest(r.Body, types.MaxStreamingBodyBytes, &req); err != nil {
    ...
    default:
        writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
```
```go
// pkg/types/contract_validation.go:70-72
dec := json.NewDecoder(&buf)
dec.DisallowUnknownFields()
```
A contract test pins this behaviour: `cmd/cppworker/streaming_byte_format_test.go:421-429` (`TestV1ChatCompletions_StrictDecode_RejectsUnknownField`).

**Explanation.** OpenWebUI's Ollama connection sends `options.top_k` whenever the user touches Top K (and its "advanced params" default includes top_k). The balancer rewrites the request to `/v1/chat/completions`, injects `top_k`, and cppworker answers `400 {"error":"invalid JSON: json: unknown field \"top_k\""}`. The balancer then passes that 400 through (`llamacpp_transport.go:454-470` for streaming, `llamacpp_transport_nonstream.go:388-405` for non-streaming), so the client sees a hard failure with a confusing "invalid JSON" message even though the client's JSON was valid Ollama.

The balancer's own test suite **enshrines the bug as intended**: `internal/balancer/translation_fidelity_test.go:131` asserts `{"top_k", 40, "options.top_k → top-level top_k"}` and `:554` lists `"options.top_k": "top_k"` as a "confirmed translation". No test exercises it end-to-end against cppworker's decoder.

**Fix.** Either (a) drop `top_k` in `translateOllamaChatToOpenAI` and pass it via a side-channel the upstream understands (recommended: extend the upstream `openAIChatCompletionRequest` with `TopK *int \`json:"top_k,omitempty"\`` and wire it to `params.TopK` — the bridge already supports it, `c/bridge/bridge.go:905`), or (b) at minimum gate the injection on a capability probe so unsupported upstreams never see it. Add an end-to-end test that decodes the translator output into a `DisallowUnknownFields` mirror of `openAIChatCompletionRequest`.

---

#### C2. `options.presence_penalty` / `options.frequency_penalty` / `options.stop` reach the OpenAI path where they are partial or ill-typed

**Evidence**
```go
// internal/balancer/llamacpp_translate_req.go:154-156
if stop, ok := options["stop"]; ok {
    openaiReq["stop"] = stop
}
```
Upstream expects an **array**:
```go
// cmd/cppworker/handlers_openai.go:75
Stop        []string `json:"stop,omitempty"`
```
Ollama's documented `options.stop` may be a string (single stop) or an array (`https://raw.githubusercontent.com/ollama/ollama/main/docs/api.md`, "Generate request (With options)": `"stop": ["\n", "user:"]`). A client sending `"stop": "\n\n"` yields `json: cannot unmarshal string into Go struct field openAIChatCompletionRequest.stop of type []string` → 400.

`presence_penalty` / `frequency_penalty` are simply not translated here (they are documented as "silently dropped" in `internal/balancer/translation_fidelity_test.go:616-621`), so those OpenWebUI sliders silently do nothing on a llama.cpp backend even though `generateRequest` (types.go:21-22) supports them.

**Fix.** Normalize `stop` to `[]string` before emitting (`if s,ok := stop.(string); ok { stop = []string{s} }`). Map `presence_penalty`/`frequency_penalty` only if the target advertises support, otherwise strip them; or route Ollama-native traffic to cppworker's native `/api/chat` (`cmd/cppworker/router.go:39`) instead of the OpenAI funnel.

---

#### C3. `/api/generate` `options.stop` as a string breaks `/v1/completions`

**Evidence**
```go
// internal/balancer/llamacpp_translate_req.go:193-195
if stop, ok := options["stop"]; ok {
    openaiReq["stop"] = stop
}
```
```go
// cmd/cppworker/handlers_openai.go:41
Stop             []string `json:"stop,omitempty"`
```
Same defect as C2 on the completions path (Aider, Continue, `ollama run` with a string stop, Hermes).

**Fix.** Same normalization; add a translator unit test with `"options":{"stop":"\n"}`.

---

#### C4. `thinking` is emitted in the wrong place for `/api/generate` streaming

**Evidence**
```go
// internal/balancer/llamacpp_translate_resp.go:1183-1185
if reasoningStr != "" {
    ollamaChunk["thinking"] = reasoningStr
}
```
Ollama's `thinking` field lives **inside `message`**, and `message` exists only for `/api/chat`. For `/api/generate` the documented response objects are `{model, created_at, response, done, ...}` plus final stats — there is no `thinking` key at the top level (see the Ollama API doc's `/api/generate` examples). So a reasoning model served over `/api/generate` streams a field no client reads, and the reasoning text is lost.

Contrast with the `/api/chat` path, which is correct: `internal/balancer/llamacpp_translate_resp.go:966-971` sets `msg["thinking"]`.

**Fix.** For `/api/generate`, either drop `thinking` (documented lossy) or fold reasoning into `response`/a `message`-style envelope — but do not invent a top-level key. Prefer routing reasoning models' `/api/generate` to cppworker's native `/api/generate` (`cmd/cppworker/handlers_generate.go`) which sets its own `thinking`.

---

#### C5. Tool-call deltas are replaced by the *entire accumulated* tool-call list on every chunk

**Evidence**
```go
// internal/balancer/llamacpp_transport.go:733-739
} else {
    if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
        if choice, ok := choices[0].(map[string]interface{}); ok {
            if delta, ok := choice["delta"].(map[string]interface{}); ok {
                accumulateToolCallsFromDelta(delta, toolAccum)
```
```go
// internal/balancer/llamacpp_transport.go:948-957
if contentToolCallsProcessed || len(toolAccum) > 0 {
    if !streamCompleted {
        streamCompleted = true
        errFwd := writeStreamingSSEDone(w, originalPath, modelFromCtx, toolAccum, ...)
```
On the SSE→NDJSON path (`/api/chat`) the individual `delta.tool_calls` chunks are *suppressed* (`continue` at `:968`) and never forwarded — only the final aggregated `message.tool_calls` is emitted, as a **single non-delta object with the complete `arguments` string**:
```go
// internal/balancer/llamacpp_transport_helpers.go:220-229
tcMap := map[string]interface{}{
    "type":     "function",
    "function": acc.function,
}
if acc.id != "" {
    tcMap["id"] = acc.id
}
```
Clients that consume Ollama streaming tool calls (`ollama-python`'s `chat(stream=True)`, Cline in Ollama mode, Roo) receive one fat chunk at the end rather than incremental deltas. Worse, for the SSE→SSE passthrough on `/v1/chat/completions`, when the delta has no content (a pure `delta.tool_calls` chunk) the code path at `:776-783` calls `extractToolCallsFromSSEContent`, which returns `nil` for a chunk whose `content` is absent (`llamacpp_toolcall_detector.go:622-624`: `content, ok := delta["content"].(string); if !ok || content == "" { return nil }`), and the code then writes the **original unmodified `data`** (`:750-760`) while *also* accumulating it — so a client can observe both the raw incremental deltas and, potentially, a duplicated aggregate.

**Fix.** For `/v1/chat/completions` forward tool-call deltas verbatim (do not suppress/aggregate unless the client asked for non-streaming). For `/api/chat`, emit each delta as it arrives and only use the accumulator for the final `done` chunk's `message.tool_calls` when no delta-form tool calls were emitted. Add a test with a 2-chunk split `arguments` fragment and a 2-tool-call response.

---

#### C6. `apiReverseProxy` for `/api/v1/*` shadows `LlamaCppRouter`'s `/api/v1/cppworker/config/runtime` route

**Evidence**
```go
// internal/balancer/router.go:61-63
if isAPIv1Path(r.URL.Path) {
    return p.serveAPIv1Request(w, r)
}
```
```go
// internal/balancer/llamacpp_router.go:129-134
case "/api/v1/cppworker/config/runtime":
    // Агрегированные runtime-параметры загруженных моделей ...
    lr.handleRuntimeConfig(w, r)
```
`routeRequest` returns at `router.go:62` for **any** `/api/v1/` path, so the `LlamaCppRouter` case is unreachable dead code. The WebUI's aggregated runtime view is therefore served (or not) by the API server on the API port, with different semantics than the router intended.

**Fix.** Either delete the dead case, or narrow `isAPIv1Path` to exclude `/api/v1/cppworker/` paths that the router is supposed to answer, and add a routing test asserting which handler wins for `/api/v1/cppworker/config/runtime`.

---

#### C7. `tool_choice` is parsed by the upstream but never honoured

**Evidence**
```go
// cmd/cppworker/handlers_openai.go:82-83
// ToolChoice — управление выбором инструмента ("auto", "none", "required", или {...}).
ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
```
```go
// cmd/cppworker/handlers_openai.go:289-291
chatMsgs := openAIToChatMessage(req.Messages)
if len(req.Tools) > 0 {
    chatMsgs = augmentSystemWithTools(chatMsgs, req.Tools)
}
```
`req.ToolChoice` is referenced nowhere else in `cmd/cppworker` (verified by grep: only the two lines above).

**Explanation.** The balancer faithfully forwards `tool_choice` (`llamacpp_translate_req.go:135-139`) and even *defaults* it to `"auto"` when tools are present. But because the upstream injects tool definitions into the system prompt whenever `len(req.Tools) > 0`, a client that explicitly asks `tool_choice: "none"` (a documented, real Cline/Roo/OpenAI-SDK behaviour used to suppress tools for a turn) will still get tool definitions injected and may still receive a tool call. `"required"` and named-function choices are also ignored. This is a client-visible contract violation on `/v1/chat/completions`.

**Fix.** Honour `tool_choice` upstream: on `"none"`, skip `augmentSystemWithTools` and suppress tool-call extraction; on `"required"`/named, restrict the injected definitions and force `finish_reason:"tool_calls"` semantics. Add contract tests for all three values.

---

### HIGH

---

#### H1. `/api/generate` returns no `context` and no usage counters unless the upstream sends a usage chunk

**Evidence**
```go
// internal/balancer/llamacpp_transport_helpers.go:269-287
doneChunk := map[string]interface{}{
    "model":      modelName,
    "created_at": time.Now().UTC().Format(time.RFC3339),
    "done":       true,
    "response":   responseValue,
}
switch {
case hasToolCalls:
    doneChunk["done_reason"] = "tool_calls"
case finalContent != "":
    doneChunk["done_reason"] = "stop"
}
```
No `eval_count`, `prompt_eval_count`, `total_duration`, `load_duration`, `eval_duration`, or `context`. Ollama's documented final `/api/generate` object contains all of them ("A stream of JSON objects is returned" → final response block). `context` is documented as the encoding a client echoes back to preserve conversational memory without re-sending the prompt.

The `/api/chat` variant has the same omission plus no `thinking`:
```go
// internal/balancer/llamacpp_transport_helpers.go:236-248
doneChunk := map[string]interface{}{
    "model":      modelName,
    "created_at": time.Now().UTC().Format(time.RFC3339),
    "done":       true,
    "message":    msgMap,
}
if hasToolCalls {
    doneChunk["done_reason"] = "tool_calls"
} else if finalContent != "" {
    doneChunk["done_reason"] = "stop"
}
```

**Explanation.** This path fires whenever cppworker sends a `finish_reason` without a subsequent `usage` chunk (`llamacpp_transport.go:1134-1135`, `usageChunkSeen == false`). Clients that read `eval_count`/`total_duration` from the final chunk (OpenWebUI's stats panel, `ollama-python`'s `ChatResponse`) get zeros/absent. Clients that need `context` (ollama-python `generate(context=...)` continuation, some Aider flows) cannot maintain memory and re-prefill every turn.

Also note the fabricated `done_reason: "tool_calls"` — Ollama only documents `stop` / `length` / `load` / `unload`; the client asked-form is not standard even though it happens to be tolerated.

**Fix.** Emit the full stat block in `writeStreamingSSEDone` (accumulate token counts and durations in the transport loop, mirroring `translateUsageChunkToOllama`), and map `finish_reason:"tool_calls"` → `done_reason:"stop"`. If `context` cannot be produced, document it explicitly rather than silently omitting it.

---

#### H2. Usage/token stats are *suppressed* whenever a content+`finish_reason` chunk precedes the usage chunk

**Evidence**
```go
// internal/balancer/llamacpp_translate_resp.go:631-640
if usage, hasUsage := openaiChunk["usage"].(map[string]interface{}); hasUsage && !hasNonEmptyChoices(openaiChunk) {
    if priorDoneEmitted != nil && *priorDoneEmitted {
        return nil
    }
    return translateUsageChunkToOllama(...)
}
```
`priorDoneEmitted` is set on *any* previously emitted `done:true` chunk:
```go
// internal/balancer/llamacpp_transport.go:1005-1015
if !usageChunkSeen {
    var ollamaParsed map[string]interface{}
    if err := json.Unmarshal(ollamaChunk[:len(ollamaChunk)-1], &ollamaParsed); err == nil {
        if done, _ := ollamaParsed["done"].(bool); done {
            usageChunkSeen = true
            priorDoneEmitted = true
        }
    }
}
```
The R51.3/R53.1 safety net emits `done:true` on a *content + finish_reason combined* chunk:
```go
// internal/balancer/llamacpp_translate_resp.go:955-959
isWrapper := !hasContent && !hasToolCalls && !hasReasoning
if !isWrapper {
    ollamaChunk["done"] = true
    ollamaChunk["done_reason"] = finishReason
}
```
**Explanation.** cppworker legitimately combines the last content token with `finish_reason` in one chunk (the code comments at `llamacpp_transport.go:889-897` explicitly describe this). When that happens, the *subsequent* canonical usage chunk — the only carrier of `prompt_eval_count` / `eval_count` / `total_duration` — is discarded, and the client is left with the "phantom" done chunk that has no stats. This is a regression of R60.49/R60.22. OpenWebUI then shows N/A for tokens/s and token counts, exactly the symptom the prior rounds were fixing.

**Fix.** Track "done emitted" and "stats emitted" as *separate* flags. Suppress a duplicate `done:true` only if the earlier chunk already carried `eval_count`; otherwise merge the usage stats into the already-sent done chunk's successor (emit a stats-only chunk without `done:true`, or delete the premature safety-net done when a usage chunk is known to follow).

---

#### H3. `/api/show` on an unloaded model returns no GGUF context length → Cline/Roo mis-size the model

**Evidence**
```go
// cmd/cppworker/handlers_model.go:2067-2080  (writeOllamaShowMetadataResponse)
"model_info": map[string]interface{}{
    "architecture":  family,
    "n_layers":      nLayers,
    "n_heads":       meta.NHeads,
    "n_embd":        nEmbd,
    "n_kv_heads":    meta.NKvHeads,
    "head_dim_k":    0, // не из GGUF header
    "head_dim_v":    0, // не из GGUF header
    "n_vocab":       0, // runtime-only (load required)
    "context_size":  0, // runtime-only (load required)
    "gpu_layers":    0, // runtime-only (load required)
    "kv_cache_type": "",
    "state":         "unloaded",
},
```
Real Ollama returns the metadata key that clients actually parse:
```json
"model_info": { "llama.context_length": 8192, "llama.block_count": 32, ... }
```
(from the Ollama API doc, "Show Model Information"). `cmd/cppworker` has `meta.GGUFContextLength` available (`llamacpp_metrics_poller.go:212`, `:292`) but this handler does not emit it, and the balancer's own `handleShow` implementations simply `copyResponse` the upstream body verbatim:
```go
// internal/balancer/llamacpp_handlers_admin.go:26-31
resp, err := lr.proxyHTTP(r, backendID)
if err != nil { ... }
copyResponse(w, resp)
```
```go
// internal/balancer/ollama_router_admin.go:20-25
backendID := or.findBackendWithModel(model)
if backendID == "" { backendID = or.selectAnyBackendNoHealthy }
or.proxy.proxyRequest(w, r, backendID)
```

**Explanation.** Cline and Roo Code read the model's context window from `/api/show` to compute their compaction threshold. Because the unloaded path (the common path — R44 made `/api/show` metadata-only on purpose) reports `context_size: 0` and no `*.context_length`, those clients fall back to a conservative default and compact far more aggressively than needed. OpenWebUI also uses this to display the context size.

**Fix.** Emit `meta.GGUFContextLength` in `model_info` using the real Ollama key shape (`"<arch>.context_length"` **and** a bare `context_size` for compatibility), and have the balancer fill the key if the upstream omits it (the balancer already caches GGUF max context in `llamaMetrics[].GGUFMaxContext`, `llamacpp_metrics_poller.go:335-337`).

---

#### H4. `keep_alive` is not translated — clients cannot unload or extend a model, and `done_reason:"unload"` never appears

**Evidence**
```go
// internal/balancer/llamacpp_translate_req.go:84-158  (translateOllamaChatToOpenAI)
openaiReq := map[string]interface{}{ "model": ollamaReq["model"] }
...
```
`keep_alive` is never copied. Neither is it in the completions translator (`:163-198`) or the embeddings translator (`:202-216`). Upstream's OpenAI structs have no `keep_alive` field either (`cmd/cppworker/handlers_openai.go:30-86`), while the native Ollama struct does (`cmd/cppworker/types.go:39`, `:60-64`).

**Explanation.** Ollama's documented unload idiom is `{"model":"x","keep_alive":0}` → `{"done_reason":"unload"}`. OpenWebUI's "unload model" button and the documented "keep model loaded for 1h" both rely on it. On a llama.cpp backend the field is dropped, so the request degrades into a normal generation/unload-less call: the model stays resident, `expires_at` bookkeeping never happens, and `/api/ps` (H5) cannot report a TTL. `ollama-python`'s `client.generate(model, keep_alive=0)` likewise has no effect.

**Fix.** Either translate `keep_alive` into a balancer-side unload decision (the balancer already owns unload via `model_management.go:1163-1169`, `executeUnload` sends `keep_alive:"0s"` to a *native* endpoint — reuse that path), or forward `keep_alive` to the upstream native endpoint instead of the OpenAI funnel. At minimum, when `keep_alive == 0` and the prompt is empty, short-circuit to the balancer's unload and answer `{"done":true,"done_reason":"unload"}`.

---

#### H5. `/api/ps` returns zero-value timestamps and empty digests; models look already-expired

**Evidence**
```go
// internal/balancer/llamacpp_handlers_readonly.go:441-449
allProcesses = append(allProcesses, OllamaProcess{
    Name:     m.Name,
    Model:    m.Name,
    Size:     int64(m.Size),
    Digest:   "",
    Details:  details,
    ExpiresAt: time.Time{},
    SizeVRAM: int64(m.VRAMUsage) * 1024 * 1024,
})
```
`OllamaProcess.ExpiresAt` is a `time.Time` (`ollama_router_tags.go:34`), so `time.Time{}` marshals to `"0001-01-01T00:00:00Z"`. Ollama's documented `/api/ps` returns a real RFC3339 `expires_at`.

**Explanation.** Clients that compute remaining TTL (`expires_at - now`) see a hugely negative duration and may treat the model as evicted; the monitor shows "loaded since year 1". `digest` is empty for every running model, so any client keying on digest (dedupe, manifest matching) degrades. Note `handlePS` in the **OllamaRouter is unreachable**: `Route` explicitly returns `false` for `/api/ps` (`ollama_router.go:49-54`), so `ollama_router_tags.go:104-142` is dead code — the `/api/ps` contract is entirely owned by the llama.cpp implementation above.

**Fix.** Populate `ExpiresAt` from the balancer's own unload scheduler (`unload_scheduler.go`) or the model's `LoadedAt` + configured TTL; emit a `digest` derived from the model file (or omit the key entirely rather than sending `""`). Delete the dead `OllamaRouter.handlePS`.

---

#### H6. `/api/tags` for llama.cpp backends omits `digest` and all `details` for the loaded-model branch

**Evidence**
```go
// internal/balancer/llamacpp_handlers_readonly.go:54-62
for _, m := range lm.LoadedModels {
    if _, exists := uniqueModels[m.Name]; !exists {
        uniqueModels[m.Name] = OllamaTag{
            Name:  m.Name,
            Model: m.Name,
            Size:  0,
        }
    }
}
```
Compare the on-disk branch, which does fill size/modified_at (`:84-91`), and the fallback which fills `details` (`:266-297`). So a **loaded** model gets `size:0`, no `digest`, no `modified_at`, no `details` — and because loaded models are inserted first and the map is not overwritten, the richer on-disk entry for the same name is discarded (`if _, exists := uniqueModels[name]; !exists`).

There is no `/api/tags` equivalent of `handlePS`'s R60.58 comment; the same class of bug (hardcoded zero) was fixed for `/api/ps` but not here.

**Explanation.** OpenWebUI's model list shows size 0 B and missing quantisation/family for every loaded model, and re-fetches `/api/show` for each. `ollama-python`'s `ListResponse.models[i].digest` is `""`.

**Fix.** Merge rather than first-wins: prefer the entry with more populated fields. Populate `Size`, `Digest`, `ModifiedAt` and `Details` (`format:"gguf"`, `family: m.Architecture`, `quantization_level: m.Quantization`, `parameter_size`) from `types.LlamaCppModel` (all available — see `llamacpp_metrics_poller.go:273-314`).

---

#### H7. `/v1/models/{id}` is not handled by the balancer

**Evidence.** `LlamaCppRouter.Route` has cases for `/v1/models`, `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings` only (`llamacpp_router.go:136-158`); `OllamaRouter.Route` has no `/v1/*` cases at all (`ollama_router.go:37-73`). A `GET /v1/models/qwen3` therefore falls through to the main proxy flow, where `determineRequestBackendType` returns `BackendTypeLlamaCpp` (`proxy.go:426-428`) and the path is proxied verbatim.

**Explanation.** On a cppworker backend this works by luck (cppworker registers `/v1/models/`, `cmd/cppworker/router.go:97`). On a **pure Ollama** backend it is a guaranteed 404, because Ollama has no `/v1/*` API. OpenAI-SDK clients (`openai.models.retrieve(id)`), Continue, and some Cline paths call it. There is also no balancer-side aggregation, so a two-backend cluster returns whichever backend happened to be selected.

**Fix.** Register `/v1/models/` in `LlamaCppRouter` (or a dedicated OpenAI router) and synthesize a single `{"id","object":"model","created","owned_by"}` from the same union used by `handleOpenAIModels`, plus a 404 with an OpenAI-shaped error body for unknown ids.

---

#### H8. `findModelOnLlamaCppBackend` reads only one of the two metrics caches → wrong-backend routing / missed warm affinity

**Evidence**
```go
// internal/balancer/llamacpp_backend_helpers.go:89-93
for _, m := range metrics.LlamaCpp.LoadedModels {
    if m.Name == model {
        return id
    }
}
```
But the poller writes to `llamaMetrics`:
```go
// internal/balancer/llamacpp_metrics_poller.go:318-324
p.proxy.metricsMgr.mu.Lock()
lm, ok := p.proxy.metricsMgr.llamaMetrics[b.id]
if !ok { lm = &types.LlamaCppMetrics{}; p.proxy.metricsMgr.llamaMetrics[b.id] = lm }
lm.LoadedModels = loadedModels
```
The correct pattern is used elsewhere:
```go
// internal/balancer/slot_manager.go:188-206
p.metricsMgr.mu.RLock()
hasModel := false
if metrics, ok := p.metricsMgr.metrics[id]; ok { if p.backendHasModel(metrics, model) { hasModel = true } }
if !hasModel {
    if lm, ok := p.metricsMgr.llamaMetrics[id]; ok && lm != nil { ... }
}
```
`metrics[id].LlamaCpp.LoadedModels` is populated only by an Ollama agent (`proxy.go:1069-1071`), which a pure-cppworker deployment does not have.

**Explanation.** `handleOpenAIChatCompletions`, `handleOpenAICompletion`, `handleOpenAIEmbeddings`, `handleChat`, `handleGenerate` all use `findModelOnLlamaCppBackend` first and fall back to `selectAnyLlamaCppHealthy()` (`llamacpp_handlers_inference.go:51-54`, `:287-290`, `:482-485`, `:581-584`, `:711-714`). So in a multi-backend cluster the balancer cannot tell which worker already holds the model and may route to a cold worker, paying a full load latency on requests the client expected to be warm. Consequence for clients: intermittent multi-minute first-token latency and OpenWebUI/Cline timeouts.

**Fix.** Check `llamaMetrics` first (and both caches, like `findModelOnAnyBackendNoVRAMCheck`), with the same fuzzy match used by `IsLlamaCppModelLoaded` (`num_ctx_resolver.go:441-449`).

---

#### H9. Substring model matching can select a backend that does not have the requested model

**Evidence**
```go
// internal/balancer/backend_selector.go:443-456
func (p *Proxy) backendHasModel(metrics *types.BackendMetrics, modelName string) bool {
	for _, m := range metrics.Ollama.RunningModels {
		if m.Name == modelName || strings.Contains(m.Name, modelName) {
			return true
		}
	}
	for _, m := range metrics.LlamaCpp.LoadedModels {
		if m.Name == modelName || strings.Contains(m.Name, modelName) {
			return true
		}
	}
```
Used by `findModelOnAnyBackendNoVRAMCheck` (`slot_manager.go:192`) which the read/mgmt path relies on (`proxy.go:800`), and by `findLessLoadedBackendWithModel` (`backend_selector.go:360`).

**Explanation.** `strings.Contains(m.Name, modelName)` means a request for model `"4"` matches `"gemma-4-E4B"`, and a request for `"qwen3"` matches `"qwen3-30b"` while the user asked for `"qwen3-4b"`. The read/mgmt path (which skips warmup and VRAM checks) will pin the request to an arbitrary backend whose loaded model merely *contains* the string, and `X-Backend-ID` will report it. On a cluster with several similarly named GGUF files this yields 404s and confusing "model not found on any backend" errors from `/api/delete` (`ollama_router_admin.go:45-49` uses the strict variant, but `findModelOnAnyBackendNoVRAMCheck` does not).

**Fix.** Make substring matching opt-in and ordered by specificity; prefer exact `EqualFold` on the canonical name and the `basename` before falling back to `Contains`. `backendHasModelStrict` (`:460`) already exists — use it on the routing path and reserve `backendHasModel` for diagnostics.

---

### MEDIUM

---

#### M1. `options.num_ctx` is dropped from the upstream body (only carried in a header)

**Evidence.** `translateOllamaChatToOpenAI` never copies `options.num_ctx` (see the whole function, `llamacpp_translate_req.go:84-158`); the only `num_ctx` handling is the side-channel header:
```go
// internal/balancer/num_ctx_resolver.go:389-397
func (p *Proxy) ApplyCppCtxHeader(r *http.Request, modelName string, bodyBuf []byte, backendID string) ResolvedNumCtx {
	resolved := p.ResolveNumCtx(modelName, bodyBuf, backendID)
	if resolved.Value > 0 {
		if r.Header == nil { return resolved }
		r.Header.Set("X-Cpp-Ctx", strconv.Itoa(resolved.Value))
	}
	return resolved
}
```
The header is only set by the llama.cpp router handlers (`llamacpp_handlers_inference.go:141`, `:357`, `:649`, `:771`). It is **not** set on the main proxy flow path (`proxy.go:885` → `proxyRequest`), which is the path taken for `/api/chat` in mixed mode (`router.go:90-94`).

**Explanation.** For a mixed cluster serving `/api/chat` through the main flow, `num_ctx` reaches cppworker only if that backend's default or profile happens to match. Cline's `num_ctx` from settings (`options.num_ctx`) is silently ignored. When the header *is* set, the body still says nothing, so any code reading `num_ctx` from the body (logging, telemetry, `ExtractNumCtxFromBody` at `llamacpp_transport.go:98`) works but an operator debugging the wire sees no `num_ctx` — and `auto_continue.go:530-545` had to work around this by hardcoding `max_tokens` because the upstream rejects Ollama-only fields.

**Fix.** Also write the resolved value into the upstream body as OpenAI `num_ctx` — the upstream OpenAI struct already accepts it (`cmd/cppworker/handlers_openai.go:79` — `NumCtx int \`json:"num_ctx,omitempty"\``) and wires it to `params.NCtxOverride` (`:330-332`). That removes the header dependency entirely.

---

#### M2. Streaming non-`error` failures produce a body with **no** `message` key at all

**Evidence**
```go
// internal/balancer/streaming.go:1201-1211
truncatedChunk := map[string]interface{}{
    "model":       modelFromCtx,
    "created_at":  time.Now().UTC().Format(time.RFC3339),
    "done":        true,
    "done_reason": "truncated",
    "error":       "stream truncated by upstream before completion",
}
out, _ := json.Marshal(truncatedChunk)
```
and
```go
// internal/balancer/streaming.go:427-432
func (p *Proxy) SendNDJSONErrorSafe(w http.ResponseWriter, flusher http.Flusher, message, backendID string) {
	donePayload, _ := json.Marshal(map[string]interface{}{
		"done":    true,
		"error":   "backend_read_error",
		"message": message,
	})
```
Here `"message"` is a **string**, not the `{role,content}` object the Ollama schema requires.

**Explanation.** Ollama-compatible parsers (ollama-python's `ChatResponse.message`, ollama-js) construct/destructure `message` as an object. A string `message` will either fail deserialisation or be dropped, and the trailing `done:true` without a `message` object can surface as "empty response". `writeStreamErrorChunk` (`llamacpp_handlers_inference.go:884-896`) gets this right, so the two code paths disagree.

**Fix.** Route both through `writeStreamErrorChunk` so `/api/chat` failures always carry `message:{role:"assistant",content:""}` (+ `reasoning` when applicable), and never overload `message` with an error string. Use `error` for the message and keep the schema shape.

---

#### M3. `filterOpenAIStreamingLine` appends an extra `\n` — malformed SSE frames on the passthrough path

**Evidence**
```go
// internal/balancer/llamacpp_content_filter.go:236-237
// Возвращаем как `data: {...}\n\n` (с финальным \n для совместимости с SSE-форматом).
return append([]byte("data: "+string(out)+"\n\n"), '\n'), true
```
Input is already `data: {...}\n\n` (tests at `proxy_request_openai_test.go:330` supply exactly that). The result is `data: {...}\n\n\n`.

**Explanation.** Strict SSE clients (aiohttp-backed OpenWebUI, `httpx`-based SDKs) treat a blank line as the event terminator and a following blank line as a new (empty) event; most tolerate it, but `sse-starlette`/`eventsource-parser` variants log/raise on empty events, and it is pure protocol noise on the hot path for every filtered Gemma/Llama service-token chunk.

**Fix.** `return []byte("data: " + string(out) + "\n\n"), true` and normalise the trailing-newline count instead of appending.

---

#### M4. Content chunks containing a service token are blanked entirely (text loss)

**Evidence**
```go
// internal/balancer/llamacpp_content_filter.go:209-221
if !shouldFilterLlamaCppContent(content) { return line, false }
// content — служебный токен. Заменяем на пустую delta...
filteredChunk := map[string]interface{}{
    "choices": []map[string]interface{}{ {"index": 0, "delta": map[string]interface{}{}} },
}
```
```go
// internal/balancer/llamacpp_content_filter.go:161-165
for _, tok := range filteredTokens {
    if strings.Contains(lower, tok) { return true }
}
```
with the test asserting the intent: `internal/balancer/proxy_request_openai_test.go:305` — `{"eot_in_middle", "hello<|eot_id|>world", true}` and `:350-359` documenting "весь чанк фильтруется".

**Explanation.** Any chunk whose content merely *contains* a token substring is replaced by an empty delta, so `"hello<end_of_turn>world"` loses all 21 characters instead of the 14-byte token. Since `shouldFilterLlamaCppContent` uses `Contains` (not equality), this also fires on legitimate text such as a user asking a model to discuss `<|im_end|>` or a code block containing `"<eos>"`. This is unconditional content data loss on the client-visible stream.

**Fix.** Strip only the token and keep the remainder (`delta.content = strings.ReplaceAll(content, tok, "")`), and only fully blank the chunk when the remainder is empty. Keep the full-blank behaviour as an opt-in.

---

#### M5. `deduplicateResponseContent` deletes legitimate repeated content

**Evidence**
```go
// internal/balancer/llamacpp_translate_resp.go:225-267
anchorLen := 80
anchor := content[:anchorLen]
...
if pos := strings.Index(content[searchStart:], anchor); pos != -1 { firstDuplicatePos = ... }
...
trimmed := content[:cutPos]
```
Called unconditionally on the final chat content:
```go
// internal/balancer/llamacpp_translate_resp.go:801-808
dedupedContent, removedBytes := deduplicateResponseContent(accumulatedContent)
...
ollamaChunk["message"] = map[string]interface{}{"role": "assistant", "content": dedupedContent}
```
**Explanation.** If the first 80 characters of an answer recur anywhere later, everything from the second occurrence onward is deleted. Legitimate cases: an assistant that repeats an opening line in two sections, a generated file that starts with the same 80-byte license header twice, a translation task that outputs the same header per language, a table with a repeated first cell. The heuristic is not restricted to models known to restart, and there is no config gate. This is content truncation from the client's point of view — arguably worse than duplication.

**Fix.** Gate the dedup behind an allow-list of models/config flag, narrow the anchor (e.g. require it to start at a `\n` and require ≥2 full repeats), and log/metric every trim. Never silently shorten content.

---

#### M6. Async n_ctx reload answers with a bespoke error schema for both API dialects

**Evidence**
```go
// internal/balancer/llamacpp_transport.go:82-89
w.Header().Set("Content-Type", "application/json")
w.Header().Set("Retry-After", "30")
w.Header().Set("X-NCtx-Reload-Decision", "async-reload")
body := []byte(fmt.Sprintf(`{"error":%q,"decision":"async_reload","retry_after_seconds":30}`,
    preflightMsg))
```
Same shape in `llamacpp_transport_nonstream.go:78-85`.

**Explanation.** For `/v1/*` clients (Cline, `openai-python`) the spec-conformant body is `{"error":{"message":...,"type":...,"code":...}}`; for `/api/*` clients it is `{"error":"..."}`. This body is neither — it is a bare `error` **string** plus non-standard sibling keys. `openai-python` raises a generic `APIError` without a parseable `type`; Hermes/Cline cannot distinguish "retry" from "bad request". The code already knows the right shapes (`llamacpp_transport_nonstream.go:410-434` builds both properly) but does not use them here.

**Fix.** Reuse the `isOpenAIPath(originalPath)` branch from `llamacpp_transport_nonstream.go:410` to pick the error envelope in both async-503 sites.

---

#### M7. `X-Cpp-Ctx` (resolved num_ctx) is not propagated by the OpenAI auto-stream workaround

**Evidence**
```go
// internal/balancer/proxy_request_openai_auto_stream.go:87-96
upstreamReq.Header.Set("Content-Type", "application/json")
upstreamReq.Header.Set("Accept", "text/event-stream, application/json")
upstreamReq.ContentLength = int64(len(upstreamBody))
if xff := r.Header.Get("X-Forwarded-For"); xff != "" { upstreamReq.Header.Set("X-Forwarded-For", xff) }
if xri := r.Header.Get("X-Real-IP"); xri != "" { upstreamReq.Header.Set("X-Real-IP", xri) }
```
Only XFF/X-Real-IP are copied — unlike `llamacpp_transport.go:188-196` and `llamacpp_transport_nonstream.go:149-157`, which copy the whole incoming header set.

**Explanation.** `handleOpenAIChatCompletions` sets `X-Cpp-Ctx` at `llamacpp_handlers_inference.go:141` *before* branching into the auto-stream path at `:158-172`, but the auto-stream request then drops it. With `LB_OPENAI_AUTO_STREAM=true` (documented for slow CPU models) every non-streaming `/v1/chat/completions` loses the resolved `num_ctx`/n_ctx override and falls back to the backend default — exactly the Cline `num_ctx` scenario the resolver exists to serve.

**Fix.** Copy `r.Header` minus hop-by-hop headers (mirroring the other two transports), or explicitly copy `X-Cpp-Ctx` / `X-Cpp-*` forwarded headers.

---

#### M8. Auto-stream non-stream response can carry an empty `id` and lacks `system_fingerprint`

**Evidence**
```go
// internal/balancer/proxy_request_openai_auto_stream.go:224-231
func newOpenAIStreamAccumulator(model string) *openAIStreamAccumulator {
	return &openAIStreamAccumulator{
		model:     model,
		created:   time.Now().Unix(),
		object:    "chat.completion",
		startedAt: time.Now(),
	}
}
```
`id` is only filled if an upstream chunk carried one (`:239-241`). `toNonStreamResponse` then emits `"id": a.id` (`:305-309`).

**Explanation.** If cppworker's SSE chunks omit `id` (they do in the balancer's own tests, e.g. `proxy_request_openai_test.go:229`), the synthesized non-stream response has `"id":""`. OpenAI-SDK clients use `id` for logging/dedup; an empty id is unusual. `system_fingerprint` is entirely absent (also absent from the streaming passthrough, which is more defensible).

**Fix.** Generate a fallback id (`"chatcmpl-"+<request id>`) when empty, and consider emitting a stable `system_fingerprint` from the request id.

---

#### M9. `/api/embed` gets the wrong error envelope and no response translation for llama.cpp backends

**Evidence**
```go
// internal/balancer/llamacpp_translate_req.go:11-22
func translatePathForLlamaCpp(ollamaPath string) string {
	switch ollamaPath {
	case "/api/chat":      return "/v1/chat/completions"
	case "/api/generate":  return "/v1/completions"
	case "/api/embeddings": return "/v1/embeddings"
	default: return ollamaPath
	}
}
```
`/api/embed` is not in the table, so it passes through as `/api/embed` (which cppworker does serve — `cmd/cppworker/router.go:41`), but for **non-2xx** responses the Ollama-shaped wrapper is chosen by `isOpenAIPath(originalPath)`:
```go
// internal/balancer/llamacpp_transport_nonstream.go:410-434
if isOpenAIPath(originalPath) { ... } else {
    errBody, _ = json.Marshal(map[string]interface{}{
        "model": modelFromCtx, "created_at": ..., "done": true, "done_reason": "error", ...
    })
}
```
`/api/embed` is not under `/v1/`, so a 5xx yields a *chat-shaped* `{done,done_reason,message}` body for an embeddings endpoint. Conversely, upstream 4xx is passed through verbatim (`:388-405`), so the client sees whatever cppworker's `writeError` produced — note cppworker's `/api/embed` error shape is `{"error": ...}` (via `writeError`) while Ollama's is also `{"error": ...}`, so 4xx is fine but 5xx is wrong.

Also `handleOllamaEmbed` accepts `truncate` and `options` but the comment admits: *"поддержка `truncate` и `options` (пока игнорируем — truncation в cppworker не реализован)"* (`cmd/cppworker/handlers_embeddings.go:96-97`), so `truncate:false` semantics (error instead of truncate) are never enforced.

**Fix.** Add `/api/embed` to the error-envelope decision (treat `/api/*` uniformly), and either implement `truncate:false` or document the deviation in the client-facing docs.

---

#### M10. `/api/embeddings` input-kind translation silently produces an empty `input`

**Evidence**
```go
// internal/balancer/llamacpp_translate_req.go:210-214
if input, ok := ollamaReq["input"].(string); ok && input != "" {
    openaiReq["input"] = input
} else if prompt, ok := ollamaReq["prompt"].(string); ok && prompt != "" {
    openaiReq["input"] = prompt
}
```
If the client sends `"input": ["a","b"]` (array — the shape `/api/embed` documents and some clients reuse on the legacy endpoint), both branches fail and `openaiReq` has **no `input` key** at all. Upstream requires it:
```go
// cmd/cppworker/handlers_openai.go — /v1/embeddings handler validates input (see handlers_openai.go embedding path)
```
and even if it did not, the client gets a 400/500 rather than a clear translation error. There is no log line for the drop.

**Fix.** Reject with a clear error, or serialize arrays through; at minimum log when the translator drops `input`.

---

#### M11. Non-streaming `/api/chat` and `/api/generate` responses omit `eval_count`/`prompt_eval_count` when upstream has no `usage`

**Evidence**
```go
// internal/balancer/llamacpp_translate_resp.go:508-517
if usage, ok := openaiResp["usage"].(map[string]interface{}); ok {
    if completionTokens, ok := usage["completion_tokens"].(float64); ok {
        ollamaResp["eval_count"] = int(completionTokens)
    }
    ...
}
```
The whole block is conditional on `usage` being present. cppworker's `/v1/completions` non-stream response includes `usage` only when the request asked for it (`stream_options` is a *streaming* concern; the non-stream path emits `usage` — see `openAITokenUsage` at `cmd/cppworker/handlers_openai.go:1968` — but the balancer does not guarantee it). When absent, `ollamaResp` has `done:true` with no counters, and `total_duration` is set to `0` **only inside the same `if`** (`:481`), so it is missing entirely rather than zero.

**Explanation.** `ollama-python`'s `GenerateResponse.eval_count` is `None`; OpenWebUI's stats panel shows N/A. Ollama guarantees these fields on the final object.

**Fix.** Always emit the counters (0 when unknown) and always emit `total_duration`/`load_duration`, preferring balancer-measured elapsed time over 0 — the transport already tracks start time (`llamaStartTime`, `llamacpp_transport.go:51`).

---

#### M12. Streaming final chat content loses `thinking`/`reasoning` even when reasoning was streamed

**Evidence**
```go
// internal/balancer/llamacpp_transport_helpers.go:207-210
msgMap := map[string]interface{}{
    "role":    "assistant",
    "content": contentForMsg,
}
```
`writeStreamingSSEDone` has no reasoning parameter at all; the per-chunk translator does emit `thinking` (`llamacpp_translate_resp.go:966-981`), but the terminal chunk does not.

**Explanation.** Clients that reconstruct conversation history from the final message (Roo/Cline write the assistant message back into context; OpenWebUI persists messages) will not carry the model's reasoning forward, so multi-turn reasoning models lose their chain-of-thought context and behave differently on turn 2. Ollama's non-streaming chat response carries `message.thinking`; clients reasonably expect the streamed final object to carry the accumulated value too.

**Fix.** Accumulate `reasoning` alongside `accumulatedPlainContent` in `proxyRequestLlamaCpp` and pass it into `writeStreamingSSEDone` for `message.reasoning`.

---

#### M13. `handleTags` performs N synchronous upstream file scans per request with 10s timeouts

**Evidence**
```go
// internal/balancer/llamacpp_handlers_readonly.go:71-77
for _, b := range backends {
    files, err := lr.fetchLlamaCppFiles(b.host, b.port)
```
```go
// internal/balancer/llamacpp_handlers_readonly.go:183
client := &http.Client{Timeout: 10 * time.Second}
```
(also `fetchLlamaCppTags` `:227`, `fetchLlamaCppModels` `:136`, and the same pattern in `handleOpenAIModels` `:504-510`).

**Explanation.** `/api/tags` and `/v1/models` are polled aggressively by OpenWebUI (model list refresh) and by Cline/Roo (model picker). Each poll fans out one HTTP request per backend, each with a 10-second timeout, executed sequentially. With 4 backends and one hung worker the client's model list blocks for up to 40 s. This is a client-visible latency/availability bug, not just inefficiency.

**Fix.** Query backends concurrently (the OllamaRouter already does this with `sync.WaitGroup`, `ollama_router_tags.go:62-83`), cache the union for a few seconds, and bound the per-backend timeout to ~2 s.

---

#### M14. `/api/tags` can return an *empty* list while a healthy llama.cpp backend has models

**Evidence**
```go
// internal/balancer/router.go:131-144
switch {
case hasOllama:
    return []BackendRouter{p.ollamaRouter, p.llamaCppRouter}
case hasLlama:
    return []BackendRouter{p.llamaCppRouter, p.ollamaRouter}
default:
    return []BackendRouter{p.ollamaRouter, p.llamaCppRouter}
}
```
`OllamaRouter.handleTags` returns early with an empty list when no *usable* Ollama backend is found:
```go
// internal/balancer/ollama_router_tags.go:56-60
backends := or.getHealthyBackends()
if len(backends) == 0 {
    writeJSON(w, http.StatusOK, OllamaTagsResponse{Models: []OllamaTag{}})
    return
}
```
and `dispatchRouters` stops at the first router that returns `true` (`router.go:150-160`).

**Explanation.** In a mixed cluster where all Ollama backends are `unhealthy` and all llama.cpp backends are `healthy`, `hasOllama` is still true (it counts registered backends, `backend_type_filter.go:63`), so the Ollama router is tried first; `getHealthyBackends` filters on status, finds nothing, and returns `{"models":[]}` with HTTP 200. The client (OpenWebUI) concludes the cluster has **no models at all** and never tries the llama.cpp router. This is a hard availability failure presented as success.

**Fix.** Return `false` from `handleTags`/`handleVersion` when the router has no eligible backends, so `dispatchRouters` falls through; or make `buildRoutersForDispatch` count *healthy* backends per type.

---

#### M15. `PROXY` `/api/ps` reference returns `ExpiresAt` zero — same class also present in `OllamaProcess` handling for Ollama backends (dead code duplicates the bug)

**Evidence.** `ollama_router_tags.go:28-36` defines `OllamaProcess` and `:104-142` implements `handlePS`, but `ollama_router.go:49-54` returns early:
```go
case "/api/ps":
    // Round 21: don't proxy /api/ps to Ollama backend ...
    return false
```
So the Ollama `handlePS` never runs; the llama.cpp implementation (H5) is the only `/api/ps` in the product. If the early return is ever removed, the dead handler also never sets `ExpiresAt` (`ollama_router_tags.go:136-139` appends the decoded `procs` verbatim, so it is at least upstream-faithful).

**Fix.** Delete the dead handler or wire it, and document that `/api/ps` is balancer-owned.

---

#### M16. `model` echo can leak the upstream alias instead of the requested name

**Evidence**
```go
// internal/balancer/llamacpp_transport.go:699-703
if modelFromCtx == "" {
    if m, ok := chunk["model"].(string); ok && m != "" {
        modelFromCtx = m
    }
}
```
```go
// internal/balancer/llamacpp_transport_helpers.go:232-235
modelName := modelFromCtx
if modelName == "" { modelName = "unknown" }
```
Also `writeStreamingSSEDone` and `buildDoneResponse` (`llamacpp_transport_helpers.go:304-307`) fall back to the literal string `"unknown"`.

**Explanation.** When the client's requested name is non-empty, the response model is correct (good). But in the paths where `modelContextKey` was not populated (e.g. requests the balancer routes without an extracted model — `/api/show` uses `name`, `/api/embed` bodies, or a request whose body failed to parse), the client receives the **upstream's** model name, which may be the alias resolved by cppworker (`handlers_openai.go:281-285` comment: *"если dedup-pre-check загрузил модель под alias (например requested=\"Qwen3-Instruct-2507-q4km\" → loaded=\"qwen3-instruct\")"*). For a client that keys conversation state on the model string, seeing a different model name mid-stream is a protocol surprise. `"unknown"` is not a model the client ever asked for.

**Fix.** Always populate `modelContextKey` for every inference path (including `/api/embed`), and echo the client-supplied string; never emit `"unknown"`.

---

#### M17. Ollama `/api/generate` and `/api/chat` 5xx are converted to 502 with a foreign body

**Evidence**
```go
// internal/balancer/llamacpp_transport_nonstream.go:435-438
w.Header().Set("Content-Type", "application/json")
w.Header().Set("Content-Length", strconv.Itoa(len(errBody)))
w.WriteHeader(http.StatusBadGateway)
```
**Explanation.** Ollama answers upstream failures with 500 and `{"error":"..."}`. The balancer turns every non-4xx upstream failure into **502** with a composite body that includes `done:true`. Clients with retry policies keyed on 5xx-vs-4xx differ (Cline retries 502 heavily), and `done:true` on an HTTP error can make a naive client treat the failure as a successful empty completion. The 4xx path was deliberately fixed to pass through (`:371-405`, R60.10) — the 5xx path should follow the same principle.

**Fix.** Preserve the upstream status code (500/503/504) and body for `/api/*` paths, keeping only the JSON envelope guarantee.

---

#### M18. `options.min_p`, `typical_p`, `tfs_z`, `mirostat*`, `num_keep`, `repeat_last_n`, `num_gpu`, `num_thread`, `penalize_newline` are all dropped

**Evidence.** The complete translation surface of `translateOllamaChatToOpenAI` (`llamacpp_translate_req.go:141-157`) is `temperature`, `top_p`, `top_k`, `num_predict`, `stop`. The upstream native struct supports far more:
```go
// cmd/cppworker/types.go:11-30
type generateOptions struct {
	Temperature float64 `json:"temperature"`
	TopP        float64 `json:"top_p"`
	TopK        int     `json:"top_k"`
	MinP        float64 `json:"min_p"`
	TypicalP    float64 `json:"typical_p"`
	TfsZ        float64 `json:"tfs_z"`
	NumPredict  int     `json:"num_predict"`
	NumKeep     int     `json:"num_keep"`
	RepeatPenalty float64 `json:"repeat_penalty"`
	FrequencyPenalty float64 `json:"frequency_penalty"`
	PresencePenalty float64 `json:"presence_penalty"`
	RepeatLastN int `json:"repeat_last_n"`
	Mirostat int `json:"mirostat"`
	...
	Seed int `json:"seed"`
	NumCtx int `json:"num_ctx"`
	Stop interface{} `json:"stop"`
}
```
The balancer's own test documents `seed`, `repeat_penalty`, `frequency_penalty`, `presence_penalty` as "silently dropped" (`translation_fidelity_test.go:616-621`).

**Explanation.** Users tuning an Ollama model through OpenWebUI (Repeat penalty, Min P, Mirostat, Keep N) get silently different sampling on a llama.cpp backend than on a native Ollama backend. This is the single largest behavioural divergence between the two backend types, and it is invisible — no warning, no log, no header.

**Fix.** Route `/api/chat`/`/api/generate` to cppworker's **native Ollama endpoints** (`cmd/cppworker/router.go:38-39`) when the client used the Ollama API, which removes the need for translation altogether and preserves the full option set. If the OpenAI funnel must be kept, translate the whole `generateOptions` set into an extended OpenAI body and add the fields to the upstream struct, then add a table-driven fidelity test that asserts every documented Ollama option round-trips.

---

#### M19. `format` (JSON mode / structured outputs) is dropped on the Ollama path

**Evidence.** `format` appears nowhere in `llamacpp_translate_req.go`. Ollama documents `format: "json"` and JSON-schema `format` for both `/api/generate` and `/api/chat` (Ollama API doc, "Structured outputs" / "JSON mode"). The upstream OpenAI structs have no `response_format` field either (`handlers_openai.go:30-86`), despite `FieldResponseFormat` existing as a constant (`pkg/types/contract_validation.go:198`).

**Explanation.** Structured-output users (OpenWebUI's "JSON mode" toggle, programmatic agents) silently get unconstrained text. This is worse than an error because the client believes it requested a schema.

**Fix.** Implement `format` → `response_format` (or a grammar) or return an explicit unsupported-feature error. Do not drop it silently; at minimum log and set a response header.

---

### LOW

---

#### L1. NDJSON heartbeat uses a non-chunk-shaped `{"done":false}` object

**Evidence**
```go
// internal/balancer/streaming.go:144-154
} else {
    hb := map[string]interface{}{"done": false}
    hbJSON, _ := json.Marshal(hb)
    heartbeatBytes = append(hbJSON, '\n')
}
```
**Explanation.** This is valid NDJSON and `ollama-python`/`ollama-js` iterate lines and check `done`, so it is tolerated. But it is an object with *only* `done` — no `model`, no `created_at`. Strict schema validators (Pydantic models with required fields, Zod schemas) will reject it. The comment explains the original motivation (avoid resetting reasoning assembly). Note the *llama.cpp* NDJSON path does not emit heartbeats at all (only `proxyRequestOpenAIStreaming` sends SSE `: keepalive`, `proxy_request.go:742`), so this only affects the native-Ollama streaming path.

**Fix.** Emit a real SSE comment for SSE streams and, for NDJSON, prefer an empty line (which parsers skip) or repeat the last chunk's `model`/`created_at` with `done:false`.

---

#### L2. `X-Queue-Position` / `Retry-After` are set on the *final* response, not on a preliminary one

**Evidence**
```go
// internal/balancer/proxy.go:1454-1465
// Информируем клиента о позиции в очереди и ожидаемом времени
// Заголовки будут доступны клиенту вместе с финальным ответом
w.Header().Set("X-Queue-Position", fmt.Sprintf("%d", queueLen))
...
w.Header().Set("Retry-After", fmt.Sprintf("%d", estimatedWaitSec))
```
**Explanation.** The code comment acknowledges the limitation. Because headers are not flushed before `queueMgr` hands off, the client cannot observe queueing until the response is complete — so `Retry-After` is meaningless for the in-flight request (though it is honoured if the response is a 503). Not a fidelity break, but the documented behaviour is misleading for clients that implement backoff.

**Fix.** If early headers are desired, hijack/`WriteHeader(102)` is not portable; instead document that queue position is informational only and keep `Retry-After` only on 503 responses.

---

#### L3. `/api/version` returns a non-Ollama extra key and a non-Ollama version string

**Evidence**
```go
// internal/balancer/ollama_router_tags.go:150-153
response := map[string]interface{}{
    "version":        "ollamalegion-1.0.0",
    "ollamaVersions": make(map[string]string),
}
```
Ollama returns `{"version":"0.5.1"}`. Clients that parse the version as semver (`ollama-python`'s `client.version()`, OpenWebUI's "is Ollama new enough for /api/embed" checks) receive a non-semver string. The llama.cpp variant uses a different extra key (`llamaVersions`, `llamacpp_handlers_readonly.go:464-467`), so the two respond inconsistently to the same path.

**Fix.** Return a semver-shaped version (e.g. a configurable `ollamaVersionCompat` defaulting to a recent Ollama release) and move the balancer identity into an `X-Balancer-Version` header instead of an extra body key.

---

#### L4. `/api/me` is not implemented and is not in the early-404 list

**Evidence**
```go
// internal/balancer/router.go:233-243
func isUnsupportedOllamaEndpoint(path string) bool {
	switch {
	case path == "/api/signin": return true
	case path == "/api/logout": return true
	case strings.HasPrefix(path, "/api/web/"): return true
	}
	return false
}
```
`/api/me` (and the newer `/api/me/...` namespace endpoints) fall through the routers and are proxied to the backend, where cppworker has no such route → 404 with an empty/Go-default body. `ollama-python` exposes `client.me()`; some integrations call it during connection setup.

**Fix.** Add `/api/me` to `isUnsupportedOllamaEndpoint`, or implement a minimal `{"id":"ollamalegion","name":"ollamalegion","email":""}`-shaped response so probing clients get a well-formed 200/404.

---

#### L5. Non-Ollama error bodies for the early-404 path

**Evidence**
```go
// internal/balancer/router.go:73-76
w.Header().Set("Content-Type", "application/json")
w.WriteHeader(http.StatusNotFound)
w.Write([]byte(`{"error":"not_supported","message":"this endpoint is not implemented in OllamaLegion balancer","path":"` + r.URL.Path + `"}`))
```
**Explanation.** Ollama's error contract is `{"error":"<string>"}`. Here `error` is the literal string `"not_supported"` and the human message lives in a sibling key. Clients surfacing `e.response.error` show "not_supported" instead of a usable message. Path is interpolated unescaped — a path containing `"` would produce invalid JSON (low impact, but trivially fixable).

**Fix.** `{"error":"this endpoint is not implemented in the balancer: " + path}` with proper JSON marshalling.

---

#### L6. `/api/embed` acquires a concurrency slot and can be queued

**Evidence**
```go
// internal/balancer/router.go:193-205
func isReadOnlyOrMgmtEndpoint(path string) bool {
	switch path {
	case "/api/version", "/api/tags", "/api/ps", "/v1/models",
		"/api/models", "/api/models/files", "/api/show",
		"/api/pull", "/api/push", "/api/copy", "/api/delete", "/api/create":
		return true
	}
```
`/api/embed` and `/api/embeddings` are deliberately excluded (documented at `:190-192`), so they take a slot and can be queued (`proxy.go:834-882`).

**Explanation.** This is a deliberate design decision and not a fidelity bug, but it means embeddings requests can be answered with `503 Service unavailable - all backends busy` (`proxy.go:879`) during chat load, and can wait in the queue for `QueueTimeout`. Clients (OpenWebUI RAG ingestion, Continue indexing) treat that as a hard failure. Worth documenting; the dedicated `/v1/embeddings` handler was added for exactly this reason (`llamacpp_router.go:152-157`), but `/api/embed` has no such fast path.

**Fix.** Give `/api/embed` the same direct-dispatch treatment as `/v1/embeddings`, or explicitly document the 503 behaviour.

---

#### L7. `/api/blobs/:digest` has no balancer-side handling and no backend consistency

**Evidence**
```go
// internal/balancer/router.go:200-203
// /api/blobs/<digest> — upload/download
if len(path) >= len("/api/blobs/") && path[:len("/api/blobs/")] == "/api/blobs/" {
    return true
}
```
Only slot/warmup skipping is implemented. The request is then proxied to whichever backend `selectBackend`/`selectAnyHealthy` returns (`proxy.go:786-806`), and cppworker does not implement `/api/blobs` at all (grep over `cmd/cppworker` finds no route; `router.go:20-97` has none).

**Explanation.** `POST /api/blobs/:digest` (push a blob) and `HEAD /api/blobs/:digest` (check existence) are documented Ollama endpoints used by `ollama create` with GGUF files. A client doing `ollama push`/`create` through the balancer gets an upstream 404 (or a 30 s wait) and, in a multi-backend cluster, would be inconsistent even if implemented (blob lands on one worker only). Note `translateOllamaBodyToOpenAI` has no case for `/api/blobs*`, and `isManagementEndpoint` lists `/api/blobs` (`llamacpp_translate_req.go:294`) so `stream` is not stripped — cosmetic, since the body is binary.

**Fix.** Either implement blob storage at the balancer level (or fan-out to all backends) or add `/api/blobs/` to the early-404 list with a clear message, so `ollama create/push` fails fast and loudly instead of 30-s timing out.

---

#### L8. `stop` handling diverges: `options.stop` is a `interface{}` natively but `[]string` upstream

**Evidence.** `cmd/cppworker/types.go:29` — `Stop interface{} \`json:"stop"\`` (native, tolerant) vs `cmd/cppworker/handlers_openai.go:41,75` — `Stop []string` (OpenAI, strict). Already covered as C2/C3; recorded separately because it also affects *direct* `/v1/chat/completions` clients that legitimately send a string stop (some SDK wrappers do).

**Fix.** Accept both forms upstream (`json.RawMessage` + normalise).

---

#### L9. `reasoning` vs `thinking` naming is inconsistent across paths

**Evidence**
- Streaming `/api/chat` chunk: `msg["thinking"]` (`llamacpp_translate_resp.go:970`)
- Non-streaming `/api/chat`: `msgMap["reasoning"]` (`llamacpp_translate_resp.go:423-425`)
- Non-streaming SSE-collapsed `/api/chat`: `msgMap["reasoning"]` (`llamacpp_transport_nonstream.go:652-655`)
- `/api/generate` non-stream: top-level `thinking` (`llamacpp_transport_nonstream.go:643-645`)
- OpenAI auto-stream: `message["reasoning"]` (`proxy_request_openai_auto_stream.go:299-301`)

**Explanation.** Ollama's documented field for both `/api/chat` and `/api/generate` is `thinking` inside `message`. Emitting `reasoning` on the non-streaming chat path means a client that works in streaming mode loses the reasoning text in `stream:false` mode (and vice versa). OpenWebUI reads `thinking` for the "thinking" accordion.

**Fix.** Standardise on `message.thinking` everywhere for Ollama paths; keep `reasoning` only if a specific client needs it (emit both, with identical values).

---

#### L10. Dead / unused: `formatLlamaCppModelsList` reads the wrong cache and always reports `size:0`

**Evidence**
```go
// internal/balancer/llamacpp_handlers_admin.go:164-176
lr.proxy.metricsMgr.mu.RLock()
metrics, ok := lr.proxy.metricsMgr.metrics[bi.id]
lr.proxy.metricsMgr.mu.RUnlock()
if !ok { return }
for _, m := range metrics.LlamaCpp.LoadedModels {
    results <- modelInfo{ Name: m.Name, ModifiedAt: time.Now().Format(time.RFC3339), Size: 0 }
}
```
Same single-cache bug as H8, plus a fabricated `modified_at` of "now" for every model. `formatLlamaCppModelsList` appears to be unused by any registered route (no caller found in `internal/balancer`), so the impact today is nil — but it is a trap for the next maintainer and it encodes the same two mistakes.

**Fix.** Delete it, or fix the cache lookup and use the model file's real mtime.

---

#### L11. CORS `Allow-Headers` omits headers real clients send

**Evidence**
```go
// internal/balancer/proxy.go:566
w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With, Accept, Origin")
```
**Explanation.** Browser-based clients (OpenWebUI's front-end talking directly to the balancer, custom web UIs) that send `X-API-Token` (the balancer's own configured auth header, `cmd/balancer/env_auth.go`, `internal/api/auth.go`) or `X-Request-ID` will fail the preflight, because the browser blocks the request when a request header is not listed. `Access-Control-Allow-Methods` also omits `PATCH`. This is a real blocker for browser clients using token auth.

**Fix.** Echo the requested headers (`Access-Control-Request-Headers`) or include `X-API-Token, X-Request-ID, X-Client-Name` and add `PATCH`.

---

#### L12. No request-body size limit on the client-facing path

**Evidence**
```go
// internal/balancer/proxy.go:1344-1349
body, err := io.ReadAll(r.Body)
if err != nil { return result }
r.Body = io.NopCloser(bytes.NewBuffer(body))
result.RawBody = body
```
and again in `proxyRequest` (`proxy_request.go:228-237`), `handleChat` (`llamacpp_handlers_inference.go:554-563`), etc.

**Explanation.** The upstream enforces 4 MB / 32 MB (`pkg/types/contract_validation.go:104-108`) and returns 413, but the balancer buffers the whole body first with no cap. A 2 GB body will be read into memory before the upstream ever sees it. Client-visible symptom: memory pressure / OOM instead of a clean 413, and `Content-Length` is not validated.

**Fix.** Wrap with `http.MaxBytesReader` at the top of `ServeHTTP` using a configured limit, and translate the overflow into the OpenAI/Ollama 413 error shape.

---

### Test-coverage gaps that let the above through

These are findings in their own right because the task is about client compatibility regressions:

- `internal/balancer/translation_fidelity_test.go:90-182` tests the translator's **output map**, never the upstream decoder. It asserts `top_k` is present (`:131`) — i.e. it locks in C1.
- `tests/openwebui_compatibility_test.go` exercises `/api/chat`/`/api/generate` only against an **Ollama-typed** mock (`setupProxyWithMockOllama`, `NewExpandedMockServer`); the llama.cpp-typed case at `:743-767` sends `{"model","prompt","stream"}` with **no `options`**, so it never reaches the `translateOllamaChatToOpenAI` option branches.
- No test sends `options` with `top_k`/`stop`-as-string/`keep_alive`/`format` through a llama.cpp backend end-to-end.
- No test asserts `/api/show` `model_info` contains a context-length key, or `/api/ps` `expires_at` is a sane timestamp.
- `tests/tool_calls_test.go` covers the non-streaming translation but the streaming delta-shape assertions live only in unit tests of `writeStreamingSSEDone`, not over the wire.

---

## 2. Verified OK

These were checked and behave correctly; listed so the report is not read as uniformly negative.

| Area | Evidence |
|---|---|
| CORS preflight returns 204 and echoes `Origin` with credentials | `proxy.go:561-575` |
| `/health` and `/metrics` short-circuit before body parsing | `proxy.go:663-676` |
| Correlation ID echoed in `X-Request-ID`, upstream id preserved as `X-Upstream-Request-Id` | `proxy.go:552`, `proxy_request.go:392-403` |
| Upstream `X-Request-Id` is *not* allowed to overwrite the balancer's | `proxy_request.go:396-403`, `:636-645` |
| `Content-Length`/`Transfer-Encoding`/`Connection` stripped from proxied responses so Go owns framing | `proxy_request.go:386-388`, `:630-632` |
| `statusRecorder` implements `Flush` and `Hijack`, so streaming/flush works through it | `proxy.go:1422-1437` |
| `Content-Length` set explicitly on non-streaming JSON responses (aiohttp `TransferEncodingError` mitigation) | `proxy_request.go:527-529`, `llamacpp_transport_nonstream.go:697-700` |
| SSE heartbeat uses a spec-legal comment frame (`: keepalive`) so strict parsers ignore it | `proxy_request.go:740-742` |
| Final `data: [DONE]` is explicitly flushed on the SSE passthrough | `llamacpp_transport_helpers.go:154-163` |
| NDJSON translated chunks always end with `\n` (no missing final newline) | `llamacpp_translate_resp.go:818`, `:875`, `:982`, `:1012`, `:1050`, `:1068`, `:1085`, `:1095`, `:1106`, `:1132`, `:1190`, `:1195`, `:1201` |
| `created_at` is RFC3339/RFC3339Nano, and the OpenAI numeric `created` is converted | `convertCreatedToRFC3339`, `llamacpp_translate_resp.go:311-347` |
| `done_reason` set from upstream `finish_reason`; `done:true` for any non-empty finish_reason (fixes Cline "Did not receive done") | `llamacpp_translate_resp.go:456-459`, `:505-508`, `:943-959`, `:1154-1159` |
| Double-`done:true` suppression via `usageChunkSeen` / `priorDoneEmitted` | `llamacpp_transport_helpers.go:180-183`, `llamacpp_translate_resp.go:636-638` |
| Reasoning tag stripping handles `<think>`, `<|channel>thought`, `<|think>`, boundaries | `llamacpp_translate_resp.go:38-186` |
| `/api/*` 4xx errors are passed through verbatim with the upstream status (R60.10) | `llamacpp_transport.go:450-470`, `llamacpp_transport_nonstream.go:385-406` |
| `/api/generate` non-stream response collapses SSE correctly, including accumulated `thinking` | `llamacpp_transport_nonstream.go:629-690` |
| `num_ctx` extraction supports both `options.num_ctx` and top-level `num_ctx`, with float decoding | `num_ctx_resolver.go:46-91` |
| `tool_choice` forwarded when tools are present; defaulted to `auto` when omitted | `llamacpp_translate_req.go:131-140` |
| `tools` accepted both top-level and via `options.tools` | `llamacpp_translate_req.go:117-130` |
| `messages` from `prompt` fallback implemented for Ollama-native `/api/chat` | `llamacpp_translate_req.go:92-96` |
| Multi-modal `content` arrays are flattened instead of crashing the upstream | `openai_normalize.go:35-103` |
| `options.num_predict` → `max_tokens` and top-level `max_tokens` both honoured | `llamacpp_translate_req.go:106-108`, `:151-153` |
| `model` is echoed from the client-requested name on all normal inference paths | `llamacpp_transport.go:36-38`, `:991`; `llamacpp_translate_resp.go:380` |
| `X-Model-*` capability response headers | `capabilities_headers.go:19-33` |
| Graceful shutdown waits for active streams before closing HTTP servers | `cmd/balancer/main.go:495-519` |
| No `WriteTimeout` on the proxy server (long streams not truncated at 10.5 min) | `cmd/balancer/main.go:368` |
| `ReadTimeout` 30 s with 5 s TCP keepalive for fast client-cancel detection | `cmd/balancer/main.go:350`, `:572-579` |
| `/api/v1/*` requests are reverse-proxied to the API server rather than dead-ending in the queue | `router.go:18-44`, `proxy.go:321-344` |

---

## 3. Requirement → status table

Legend: **OK** = conformant; **PARTIAL** = works with caveats; **BROKEN** = client-visible defect; **MISSING** = not implemented.

| Requirement / endpoint | Status | Sev | Evidence |
|---|---|---|---|
| `GET /api/tags` (llama.cpp) | PARTIAL | HIGH | H6 — loaded models lose `digest`/`details`/`size`/`modified_at` (`llamacpp_handlers_readonly.go:54-62`); sequential N-backend fan-out M13 (`:71-77`); can return empty M14 (`ollama_router_tags.go:56-60`) |
| `GET /api/tags` (Ollama) | OK | — | `ollama_router_tags.go:50-102` |
| `GET /api/version` | PARTIAL | LOW | L3 — non-semver version + extra key; inconsistent `ollamaVersions` vs `llamaVersions` |
| `GET /api/ps` | BROKEN | HIGH | H5 — `expires_at` `0001-01-01T00:00:00Z`, `digest:""` (`llamacpp_handlers_readonly.go:441-449`); Ollama variant is dead code (`ollama_router.go:49-54`) |
| `POST /api/show` | BROKEN | HIGH | H3 — no context length for unloaded models (`cmd/cppworker/handlers_model.go:2067-2080`); balancer passes through unchanged (`llamacpp_handlers_admin.go:26-31`) |
| `POST /api/chat` stream (NDJSON) | BROKEN | CRITICAL | C1 (`top_k`), C2/C3 (`stop` type), C4 (`thinking` placement for generate), C5 (tool_call delta shape), H1 (missing stats), H2 (stats suppressed), M12 (final `thinking` lost) |
| `POST /api/chat` non-stream | BROKEN | MEDIUM | M11 (no counters without `usage`), L9 (`reasoning` vs `thinking`) |
| `POST /api/generate` stream | BROKEN | CRITICAL/HIGH | C3/C4, H1 (`context` + stats missing) |
| `POST /api/generate` non-stream | BROKEN | MEDIUM | M11, M17 (5xx → 502 with `done:true`) |
| `POST /api/embed` | PARTIAL | MEDIUM | M9 — wrong 5xx envelope for `/api/*`; `truncate`/`options` unimplemented upstream; L6 (takes a slot, can 503) |
| `POST /api/embeddings` | PARTIAL | MEDIUM | M10 — array `input` silently produces no `input`; `prompt` path OK |
| `POST /api/pull` | PARTIAL | MEDIUM | no balancer-side aggregation/validation; routed via `selectBackendByResources` (`ollama_router_admin.go:33-36`) and can pick a backend without the model; streaming status NDJSON passed through |
| `POST /api/push` | PARTIAL | LOW | requires the model to be already loaded on a backend (`ollama_router_admin.go:115-119`) → 404 for on-disk-only models; cppworker returns 501 |
| `POST /api/create` | PARTIAL | LOW | proxied without blob support; `files`-based create unusable (L7) |
| `POST /api/copy` | PARTIAL | LOW | `extractModelFromBody` reads `source` (`ollama_router.go:227-229`) but this is the *only* handler that needs it; works on llama.cpp path via `selectLlamaCppBackendByResources` which parses `model`/`name`, not `source` (`llamacpp_backend_helpers.go:110-113`) |
| `DELETE/POST /api/delete` | PARTIAL | LOW | Ollama router fans out to all backends holding the model (`ollama_router_admin.go:45-88`) — good; llama.cpp path forwards to a single backend only (`llamacpp_handlers_admin.go:62-74`) |
| `POST /api/blobs/:digest`, `HEAD /api/blobs/:digest` | MISSING | LOW | L7 — no implementation anywhere; cppworker has no route |
| `GET /api/me` | MISSING | LOW | L4 — not in the early-404 list, proxied to backend → 404 |
| `GET /v1/models` | OK | — | `llamacpp_handlers_readonly.go:479-561` (union of loaded + on-disk, `object:"list"`, `owned_by`) |
| `GET /v1/models/:id` | MISSING | HIGH | H7 — not registered by any router |
| `POST /v1/chat/completions` stream (SSE) | BROKEN | CRITICAL/HIGH | C5 (tool_call delta shape + possible duplicate), C7 (`tool_choice` ignored upstream), M3 (extra `\n` per filtered frame), M4 (content blanking), M8 (empty `id`) |
| `POST /v1/chat/completions` non-stream | PARTIAL | HIGH | auto-stream workaround drops `X-Cpp-Ctx` (M7) and can emit empty `id` (M8); default `LB_OPENAI_AUTO_STREAM=false` means no workaround at all (`proxy_request_openai_auto_stream.go:39-42`) |
| `POST /v1/completions` | PARTIAL | MEDIUM | string `stop` → 400 (C3); `suffix`/`echo`/`best_of`/`logprobs` not forwarded (`llamacpp_translate_req.go:163-198`) |
| `POST /v1/embeddings` | OK | — | dedicated direct dispatch (`llamacpp_router.go:152-157`, `llamacpp_handlers_inference.go:456-545`) |
| `options` (`temperature`, `top_p`) | OK | — | `llamacpp_translate_req.go:142-147` |
| `options.top_k` | BROKEN | CRITICAL | C1 |
| `options.num_predict` | OK | — | `:151-153` |
| `options.stop` | BROKEN | CRITICAL | C2/C3 — type not normalised |
| `options.num_ctx` | PARTIAL | MEDIUM | M1 — header-only, not on the main proxy path |
| `options.seed` | MISSING | MEDIUM | M18 — dropped (`translation_fidelity_test.go:616-621`) |
| `options.repeat_penalty` / `presence_penalty` / `frequency_penalty` | MISSING | MEDIUM | M18 |
| `options.min_p` / `typical_p` / `tfs_z` / `mirostat*` / `num_keep` / `repeat_last_n` | MISSING | MEDIUM | M18 |
| `keep_alive` | BROKEN | HIGH | H4 — dropped, so unload/TTL control is impossible |
| `tools` | OK | — | `:117-130` |
| `tool_choice` | BROKEN | CRITICAL | C7 — forwarded but ignored upstream (`tool_choice:"none"` cannot disable tools) |
| `format` (json / schema) | MISSING | MEDIUM | M19 |
| `images` | MISSING | LOW | dropped by translation; upstream `openAIChatMessage` has no `images` field (`cmd/cppworker/handlers_openai.go:94-100`) |
| `think` | MISSING | MEDIUM | not in `generateRequest`/`openAIChatCompletionRequest`; dropped in translation; reasoning is instead inferred from `reasoning_content` |
| `logprobs` / `top_logprobs` | MISSING | LOW | not in the request structs (`pkg/types/contract_validation.go:200-201` defines the constants only) |
| `template` / `system` / `raw` | MISSING | LOW | `generateRequest` has them (`cmd/cppworker/types.go:35-37`) but the OpenAI funnel has no equivalent |
| `suffix` | MISSING | MEDIUM | dropped in `translateOllamaGenerateToOpenAI` even though upstream accepts it (`handlers_openai.go:33`) |
| `context` (request) | MISSING | HIGH | H1 — not forwarded, not returned |
| `truncate` | PARTIAL | LOW | accepted upstream, documented as ignored (`handlers_embeddings.go:96-97`) |
| `stream` | OK | — | preserved in all three translators (`:97-101`, `:174-178`) and `stripStreamFlagForPath` skips management endpoints (`llamacpp_translate_req.go:259-274`) |
| Response `created_at` format | OK | — | `convertCreatedToRFC3339` (`llamacpp_translate_resp.go:311-347`) |
| Response `total_duration` / `load_duration` | PARTIAL | MEDIUM | present on the usage-chunk path (`:741-783`), absent on the `writeStreamingSSEDone` fallback (H1) and non-stream without `usage` (M11) |
| Response `eval_count` / `prompt_eval_count` | PARTIAL | HIGH | H2 (suppressed), H1/M11 (absent in fallbacks) |
| Response `done_reason` | PARTIAL | MEDIUM | H1 fabricates `"tool_calls"`; `"truncated"` is balancer-invented (`streaming.go:1205`) |
| Response `context` | MISSING | HIGH | H1 |
| Response `thinking` | BROKEN | CRITICAL | C4 (wrong location for `/api/generate`), M12 (lost on final chunk), L9 (`reasoning` naming) |
| Response `tool_calls` | BROKEN | CRITICAL | C5 — aggregated instead of delta; can be emitted twice |
| Response `usage` (OpenAI) | OK | — | passthrough from upstream; `stream_options.include_usage` defaults on upstream (`handlers_openai.go:50-54`) |
| Response `model` echo | PARTIAL | MEDIUM | M16 — correct when the model was extracted; otherwise upstream alias or `"unknown"` |
| Response `id` / `object` / `system_fingerprint` | PARTIAL | LOW | M8 — auto-stream can emit `""`; `system_fingerprint` never emitted |
| NDJSON framing (newline-terminated lines) | OK | — | see Verified OK row above |
| SSE framing (no stray frames) | PARTIAL | MEDIUM | M3 extra `\n` on filtered frames (`llamacpp_content_filter.go:237`); `: keepalive` is legal |
| Heartbeat in the right format | PARTIAL | LOW | L1 — NDJSON `{"done":false}` object rather than a comment/blank |
| No buffering that breaks streaming | OK | — | `Flush` after every write; hijack path (`proxy.go:1422-1437`, `proxy_request.go:582-587`) |
| Model-name translation / echo | PARTIAL | MEDIUM | M16 |
| Cline / Roo Code (Ollama mode) | BROKEN | CRITICAL | C1, C3, C5, H3 (context length), H8 (cold-backend routing) |
| Cline / Roo Code (OpenAI mode) | PARTIAL | HIGH | C7 (`tool_choice`), M3, M8; non-stream relies on an opt-in env flag |
| OpenWebUI (Ollama mode) | BROKEN | CRITICAL | C1 (`top_k` → 400), H4 (`keep_alive`), H6 (`/api/tags` details), H1/H2 (stats) |
| OpenWebUI (OpenAI mode) | PARTIAL | HIGH | C7, M3, M8 |
| ollama-python / ollama-js | BROKEN | HIGH | H4 (`keep_alive`), H1 (`context`/stats), H5 (`expires_at`), L3 (version), L4 (`me()`) |
| Continue / Aider | BROKEN | MEDIUM/HIGH | `suffix` dropped (FIM unusable), C3 (string `stop`), M18 (sampling options) |
| Hermes | PARTIAL | MEDIUM | C2, M18, M19 |

---

## 4. Recommended fix order

1. **C1, C2, C3** — one shared normalisation function for the Ollama→OpenAI body, plus an end-to-end test that decodes the translator output into a `DisallowUnknownFields` mirror of `openAIChatCompletionRequest`. This unblocks every OpenWebUI/Cline request that touches advanced options.
2. **C7** — honour `tool_choice` upstream, or stop advertising tool support when it cannot be honoured.
3. **H2, H1, M11** — make the terminal chunk carry real stats; separate "done emitted" from "stats emitted".
4. **H4, H5, H6, H3** — `/api/ps`, `/api/tags`, `/api/show` field fidelity (these are pure data-population fixes with data already available in `llamaMetrics`).
5. **C5, M4, M3** — streaming shape correctness (tool-call deltas, no text blanking, legal SSE frames).
6. **H8, H9, M14, M13** — routing correctness and `/api/tags` latency/availability.
7. **M1, M7, M12** — `num_ctx` on the body, header propagation in auto-stream, `thinking` on the final chunk.
8. **C6, M15, L10** — delete dead code so the routing story is truthful.

The highest-leverage structural change is to **stop rewriting Ollama-native requests into OpenAI requests**: cppworker already serves `/api/chat`, `/api/generate`, `/api/embed`, `/api/embeddings` natively (`cmd/cppworker/router.go:38-41`) with the full Ollama option set. Using those endpoints for Ollama-API traffic removes C1, C2, C3, C4, M1, M18 and M19 in one change, and confines the OpenAI funnel to clients that actually speak OpenAI.
