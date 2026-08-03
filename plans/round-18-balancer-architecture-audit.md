# Round 18: Balancer Architecture Audit

**Date**: 2026-08-03
**Status**: Planning
**Scope**: Comprehensive review of balancer architecture across all 7 user-stated concerns
**Author**: Mavis

## Executive Summary

After 17 rounds of fixes, the balancer is functionally working but has **6 architectural gaps** that affect the user's stated requirements:
safe model capability advertisement, full pass-through, multi-user session tracking, parallel capacity, explicit cancel, complete metrics.

**Key finding**: The current implementation relies on **passive propagation** (TCP FIN, ctx cancellation) instead of **active signaling** (cancel API, capability headers). This works for most cases but is fragile under load and lacks the observability needed for production diagnostics.

**Recommendation**: Round 18 will add 4 P0 features (capability advertisement, explicit cancel API, per-user parallel tracking, capability negotiation) in a single coordinated release. Estimated effort: 1-2 days, broken into 4 commits.

---

## 1. User-Stated Requirements (verbatim from session)

> "определять ее возможности - наличие размышлений обработку картинок и прочее и сразу вводить эти ограничения для клиентов что будут подключаться через балансер"

→ **Model capability detection + advertisement**

> "балансер должен полностью обеспечивать поток информации между бэкендом и клиентом"

→ **Full pass-through verification**

> "Правильно определять клиента и различать нескольких пользователей с одного клиента, например если запрос от двух человек идет со стороны OpenWebUI или CLine то вести их каждый в своем"

→ **Per-user session tracking**

> "взависимости от очереди давать работать каждому, формировать паралельные запросы по количеству возможных и необрывать сессии пока ответ не будет получен пользователем или пользователь сам его остановит"

→ **Parallel capacity-based dispatch + non-interrupting sessions**

> "что тоже важно так как клиент нажимает остановить но балансер не воспринимает и не дает данной команды бэкенду"

→ **Explicit cancel propagation client → balancer → backend**

> "комплексно оценить работу балансера справиться ли он с возложенными на него задачами и насколько корректно он отработает по распредлению запросови сессий от клиентов к бэкендам"

→ **Correctness review**

> "он в полной мере отдаствсе метрики помодели что работает в бэкенде на строну клиента"

→ **Full per-model metrics exposure to clients**

> "Сделать проверочные тесты на этот счет"

→ **Verification tests for all above**

---

## 2. Audit Methodology

Reviewed ~22,685 lines of Go in `internal/balancer/`. Read:
- `proxy.go` (1185 lines) — main ServeHTTP
- `proxy_request.go` (1094 lines) — non-streaming + OpenAI streaming path
- `llamacpp_transport.go` (969 lines) — streaming path with translation
- `llamacpp_transport_nonstream.go` (544 lines) — non-streaming path
- `llamacpp_translate_resp.go` (497 lines) — OpenAI→Ollama response
- `llamacpp_translate_req.go` — Ollama→OpenAI request
- `llamacpp_metrics_poller.go` (357 lines) — /api/models polling
- `llamacpp_handlers_inference.go` (700+ lines) — read-only inference handlers
- `llamacpp_handlers_admin.go` — admin endpoints
- `queue_manager.go` (468 lines) — overflow queue
- `session_manager.go` (241 lines) — session stickiness
- `client.go` — client name / session ID derivation
- `metrics.go` (191 lines) — /api/metrics aggregator
- `model_management.go` (1100+ lines) — load/unload model ops
- `model_instance_controller.go` — per-model state
- `model_latency_tracker.go` — per-model adaptive timeouts
- `nctx_reload.go` (700+ lines) — n_ctx adaptive reload
- `requestid.go` — X-Request-ID correlation

Reviewed cppworker side:
- `cmd/cppworker/router.go` — all routes
- `cmd/cppworker/handlers_chat.go` (840+ lines) — chat streaming
- `cmd/cppworker/handlers_generate.go` — generate streaming
- `cmd/cppworker/handlers_openai.go` — OpenAI streaming
- `cmd/cppworker/handlers_hf.go` — HF download (only `cancel` API exists)
- `cmd/cppworker/handlers_model.go` — load/unload
- `cmd/cppworker/handlers_config.go` — config update
- `cmd/cppworker/safe_stream_writer.go` — write error detection
- `cmd/cppworker/debug_last_stream.go` — last disconnect telemetry
- `cmd/cppworker/reasoning_content.go` — multi-tag parser (Round 17.2)
- `cmd/cppworker/auto_detect_reasoning_test.go` — auto-detect tests (Round 17)

Confirmed absence:
- `grep -i 'capabilit' internal/cppbackend/ cmd/cppworker/` → 0 matches
- `grep -i 'cancel\|abort\|stop.*gen\|stop.*infer' cmd/cppworker/` → only `handleHFCancel`
- `grep -i 'parallel\|simultaneous\|n_parallel' cmd/cppworker/` → only config, no active tracking

---

## 3. Audit Findings by Concern

### 3.1 Model Capability Detection

**Current state**:
- cppworker hardcodes reasoning detection via `IsReasoningModel()` (whitelist of name patterns: qwen3.5, qwen3.6, deepseek-r1, qwen3-thinking, gemma-4-thinking, seed-oss, kimi-k2)
- Round 17 added per-model `EnableReasoning *bool` + Layer 3 lazy auto-detect (64-char prefix scan for `<think>`)
- No vision detection
- No tools detection (relies on whether request includes `tools` field)
- No embeddings detection
- **No structured `Capabilities` field on the model** — exists only as scattered booleans

**Gap**: When OpenWebUI connects to balancer → cppworker, OpenWebUI sees the model as "supports_vision: false, supports_tools: false" because it has no way to learn these. User has to manually configure per-model.

**Impact**: User picks wrong model for task (e.g. sends image to text-only model), gets degraded experience.

### 3.2 Pass-through Verification

**Current state**:
- Request translation: Ollama→OpenAI via `translateOllamaBodyToOpenAI()` (path-based)
- Response translation: OpenAI→Ollama via `translateOpenAIResponseToOllama()` (3 functions: chat/generate/embeddings)
- Header pass-through: `req.Header` copied except `content-type/accept/content-length/host` (llamacpp_transport.go:117-125)
- Custom headers injected: `X-Forwarded-For`, `X-Real-IP`, `X-Session-ID`, `X-Backend-ID`, `X-Request-ID`
- Reasoning content: Round 17 split into `message.reasoning` (Ollama) and `delta.reasoning` (OpenAI streaming)
- Tool calls: 4 parser paths (native delta.tool_calls, content JSON detection, finish_reason="tool_calls", role-only+tool_calls chunk)

**Gaps** (verified by reading code):
1. **`X-Model-Capabilities` not set on response** — clients can't learn what model can do
2. **`X-Model-Max-Context` not set on response** — clients may request larger n_ctx than model supports
3. **`X-Model-Parallel` not set on response** — clients don't know if they can run parallel requests
4. **`X-Active-Generation-Id` not set on response** — for cancel (no cancel API exists anyway)
5. **No pass-through of `usage.prompt_tokens_details.cached_tokens`** — common OpenAI metric
6. **No pass-through of `system_fingerprint`** — OpenAI includes this for reproducibility
7. **No pass-through of `logprobs`** in stream chunks — needed for some clients
8. **`finish_reason="length"` not translated to Ollama's `done_reason`** — only checked `== "stop" || == "tool_calls"`. Round 15.1+ should fix this, but `finish_reason="length"` currently sets `done=true` (correct) but `done_reason="length"` (also correct, just not handled)
9. **`created_at` from OpenAI is Unix seconds** — already handled by `convertCreatedToRFC3339` ✓
10. **`temperature` echo in response** — not present in upstream, can be added from request

### 3.3 Client Detection + Per-user Session Tracking

**Current state**:
- `getClientName()` extracts from `X-Client-Name` header → User-Agent pattern (Cline/OpenWebUI/etc.) → "Unknown"
- `getSessionIDWithModel()` builds composite: `X-Client-ID + IP + model + endpoint + tab/request ID`
- This handles two users from Cline (different `X-Client-ID` if they set it) — Cline sets `X-Client-ID` per workspace
- This handles two users from OpenWebUI — but only if they use different browser sessions or OpenWebUI sets per-user headers (it does NOT, currently)
- Two users on same OpenWebUI without custom `X-Client-ID` → both share `fingerprint + clientName` session ID → ONE session, both serial requests

**Gap**: OpenWebUI users (multi-user mode) cannot be distinguished unless they all set `X-Client-ID` manually.

**Real-world impact**: OpenWebUI's "default" deployment is single-user. Multi-user OpenWebUI is rare. Cline has explicit per-workspace X-Client-ID. So this gap affects:
- Multi-user OpenWebUI installs (rare but real)
- Same user with multiple browser tabs on same model (collisions if X-Tab-ID not set)

**Recommendation**: Document the limitation, improve `getSessionIDWithModel` to also hash on `Authorization` header (different user tokens) when present.

### 3.4 Parallel Requests Based on Capacity

**Current state**:
- Backend config has `MaxConcurrentReqs` (default: configured per backend)
- Balancer tracks `ActiveReqs` per backend via `tryAcquireSlot` / `releaseSlot`
- Queue manager has `QueueMaxSize` and `QueueWorkers` (default 3-10)
- BatchedScheduler in cppworker has `defaultNParallel` (default 1, max 8)
- BUT: **`tryAcquireSlot` increments `ActiveReqs` for the entire request lifetime** — including streaming. So if user starts a stream, slot is "busy" until stream finishes or client cancels.

**Gap** (user's pain): "необрывать сессии пока ответ не будет получен пользователем или пользователь сам его остановит"

The current behavior IS what the user wants: session is not interrupted by another user's request. But the slot accounting is binary (free/busy), not token-aware.

**Real capacity = (slots used) × (active per slot)**. With `defaultNParallel=1`, only 1 actual generation per backend at a time. With `defaultNParallel=2`, up to 2 simultaneous generations per backend (using same model + shared KV cache).

**Sub-gap**: Balancer does NOT know `defaultNParallel` of the backend. It uses `MaxConcurrentReqs` from its own config, which is the upper bound. If user configures `MaxConcurrentReqs=4` but backend has `defaultNParallel=1`, the balancer queues 4 requests but only 1 generates at a time. Effective throughput: 1×.

**Sub-gap**: When stream finishes naturally, slot returns. When client cancels mid-stream, slot returns (via `defer releaseAcquired`). When client TCP-disconnects, balancer returns and defer runs. So slot accounting is correct.

### 3.5 Cancel Propagation

**Current state** (the user's stated pain):
- Balancer streaming loop: `case <-clientCtx.Done(): return nil` (proxy_request.go:630)
- This returns from the handler. The deferred `resp.Body.Close()` closes the response body.
- Go's HTTP client then sends FIN to cppworker.
- cppworker's `r.Context()` is cancelled when the connection closes.
- cppworker's `callback` checks `case <-ctx.Done(): return false` (handlers_chat.go:541-545).
- llama.cpp stops generating on next token.

**Problem 1: Latency**. From client cancel to llama.cpp stop:
- Client sends FIN to balancer
- Balancer's clientCtx fires (instant)
- Balancer returns from handler (instant)
- Deferred `resp.Body.Close()` (instant)
- Go HTTP client closes connection (sends FIN to cppworker)
- cppworker's request context fires (next read)
- Callback returns false
- llama.cpp stops on next token iteration

In practice this is 10-100ms. But during that window, llama.cpp continues generating tokens. Wasted compute. Wasted slot occupancy.

**Problem 2: No signal**. cppworker only knows "connection closed" — cannot distinguish:
- User pressed stop
- Network blip
- Container restarting
- Bug

So telemetry says "client_disconnected" but not "user_cancelled". Cannot log/alert on actual user cancellations.

**Problem 3: No cancel API**. There is no `/api/cancel` endpoint in cppworker. If user explicitly wants to cancel from outside the request (e.g. timeout, admin action), impossible.

**Problem 4: BatchedScheduler doesn't cancel**. Round 15.2 introduced BatchedScheduler for multi-slot generation. It holds `modelLock` for entire `BatchedDecode` call. If a session is queued in the batched scheduler and user cancels, the cancellation doesn't propagate to the queued slot — only to the currently-decoding session.

### 3.6 Comprehensive Correctness Review

**What works correctly**:
1. Round 17 reasoning routing (per-model override, lazy auto-detect, multi-tag parser)
2. Round 16 temperature=0 fix (`*float64` for `nil != 0`)
3. Multi-tool parser fix (`recoverToolCallsByPrefixTrimming` for `]}` → `}]` pattern)
4. n_ctx adaptive reload (preflight + Stage 5 metrics)
5. Session stickiness (with proper rebalance under load)
6. Queue manager (with max retries, requeue, force-rebalance)
7. Auto-pull (deduplicated, with slot awareness)
8. SSE heartbeat (10s interval, prevents idle disconnect)
9. EOF retry (one-time retry on EOF before first byte)
10. Connection-level retry (3× with backoff before mark unhealthy)
11. Graceful shutdown (active stream counter, save state)
12. Per-model adaptive timeouts (Profile > LatencyTracker > global)
13. CORS handling (per-origin, credentials, all methods)
14. Request ID correlation (X-Request-ID echo, propagated to context)

**What has known limitations** (honest list):
1. **Tag-based parser can't split LaTeX/markdown reasoning** (qwen3-instruct, gemma-4-it, custom fine-tunes)
2. **OpenWebUI multi-user session collisions** if X-Client-ID not set
3. **cppworker can only cancel on next token** during streaming
4. **No chunked-vision support** (images only work if cppworker specifically supports — currently it parses `images` field but doesn't pass to llama.cpp mmproj)
5. **BatchedScheduler holds modelLock during BatchedDecode** — no real parallelism for small N
6. **cancel from client takes 10-100ms to reach backend** (TCP FIN path)
7. **No model capability advertisement** to clients
8. **No metrics for active generation progress** (tokens generated so far, ETA)
9. **Cancel for batched-scheduler-queued requests is not propagated**
10. **OpenAI prompt_logprobs and logprobs fields not passed through** (cppworker doesn't support)
11. **No way to list models with their capabilities from one endpoint** (clients must probe)

### 3.7 Metrics Exposure

**Current state** (`/api/metrics` response from `BalancerMetrics.GetMetrics()`):
```json
{
  "timestamp": "...",
  "total_backends": N,
  "healthy_backends": N,
  "request_queue_depth": N,
  "request_queue_max": N,
  "request_queue_fill_percent": N,
  "prewarm_in_progress": N,
  "model_instance_count": {"model-name": count},
  "total_proxy_requests": N,
  "model_load_time_histogram": {...},
  "queue_wait_time_histogram": {...},
  "nctx_reloads_total": N,
  "nctx_rejects_total": N,
  "nctx_errors_total": N,
  ...
}
```

**Per-model from cppworker** (`/api/models` polled every 30s):
```json
{
  "count": N,
  "maxVramNCtx": N,
  "modelMaxContext": N,
  "availableVramMb": N,
  "totalVramMb": N,
  "models": [{
    "name": "...",
    "path": "...",
    "state": "loaded",
    "sizeBytes": N,
    "nLayers": N,
    "nKvHeads": N,
    "nEmbd": N,
    "headDimK": N,
    "headDimV": N,
    "contextSize": N,
    "ggufContextLength": N,
    "gpuLayers": N,
    "vramUsage": 0,  // cppworker doesn't report
    "ramUsage": 0,   // cppworker doesn't report
    "loadedAt": "..."
  }]
}
```

**Critical gaps**:
1. **`vramUsage` and `ramUsage` are always 0** from cppworker (only `loadingSizeBytes` is set). Even though cppworker has the data, it's not exposed.
2. **No `n_parallel` reported per model** — needed for parallel capacity
3. **No `capabilities` per model** — reasoning/vision/tools/embeddings
4. **No live generation count per model** — "X users currently generating on model Y"
5. **No `tokens_per_sec` per model** — only historical aggregation in ModelLatencyTracker (not exposed)
6. **No `activeUsers` per model** — "X distinct users on this model now"
7. **No `lastRequestAt` per model** — "model hasn't been used in 5 min"
8. **No `totalTokensGenerated` per model** — lifetime counter
9. **No `reasoningActive` per model** — "currently emitting reasoning" (for WebUI dashboard)
10. **QueueManager pending/processing lists not exposed** — should add `/api/v1/queue/pending` and `/api/v1/queue/processing`

---

## 4. Proposed Design (Round 18)

### 4.1 P0: Model Capability Advertisement

**Goal**: Clients learn what a model can do from a single API call.

#### 4.1.1 Add `Capabilities` to `LlamaCppModel`

**File**: `pkg/types/llama_cpp_metrics.go`

```go
type ModelCapabilities struct {
    Reasoning   bool `json:"reasoning"`   // emits <think> or similar
    Vision      bool `json:"vision"`      // accepts image input
    Tools       bool `json:"tools"`       // supports function calling
    Embeddings  bool `json:"embeddings"`  // produces embeddings
    CodeCompletion bool `json:"codeCompletion"` // FIM-capable
    MaxContext  int  `json:"maxContext"`  // ggufContextLength
    Parallel    int  `json:"parallel"`    // n_parallel slots available
}

type LlamaCppModel struct {
    Name         string `json:"name"`
    // ... existing fields ...
    Capabilities ModelCapabilities `json:"capabilities"`
}
```

#### 4.1.2 Auto-detect in cppworker

**File**: New `cmd/cppworker/capabilities.go`

```go
func DetectCapabilities(modelName string, modelPath string, nParallel int) ModelCapabilities {
    cap := ModelCapabilities{
        MaxContext: extractGGUFContextLength(modelPath),
        Parallel:   nParallel,
    }
    
    nameLower := strings.ToLower(modelName)
    pathLower := strings.ToLower(modelPath)
    
    // Reasoning: existing IsReasoningModel + per-model EnableReasoning
    cap.Reasoning = IsReasoningModel(modelName) || isSoftPromptReasoningEnabled(modelName)
    
    // Vision: GGUF has mmproj file in same dir
    cap.Vision = hasMMProjFile(modelPath)
    
    // Tools: chat template has tool_call placeholder
    cap.Tools = chatTemplateHasTools(modelName)
    
    // Embeddings: architecture is bert-class
    cap.Embeddings = isEmbeddingsModel(modelName, pathLower)
    
    // Code completion: FIM-capable architecture
    cap.CodeCompletion = isFIMCapable(modelName, pathLower)
    
    return cap
}

func hasMMProjFile(modelPath string) bool {
    dir := filepath.Dir(modelPath)
    baseName := strings.TrimSuffix(filepath.Base(modelPath), ".gguf")
    patterns := []string{
        filepath.Join(dir, baseName+".mmproj*.gguf"),
        filepath.Join(dir, "*mmproj*.gguf"),
    }
    for _, p := range patterns {
        if matches, _ := filepath.Glob(p); len(matches) > 0 {
            return true
        }
    }
    return false
}
```

#### 4.1.3 Expose via balancer response headers

**File**: `internal/balancer/llamacpp_transport.go` (or new `llamacpp_capability_headers.go`)

After successful model resolution, set:
```
X-Model-Capabilities: reasoning,tools,embeddings
X-Model-Max-Context: 32768
X-Model-Parallel: 2
X-Model-Architecture: qwen3
```

This allows OpenWebUI/Cline to:
- Auto-enable reasoning toggle when `reasoning=true`
- Show "Vision model" badge when `vision=true`
- Disable tools when `tools=false`
- Choose appropriate model for the task

#### 4.1.4 Add to /api/v1/gguf/backends response

**File**: `internal/balancer/llamacpp_handlers_readonly.go`

Each loaded model in the response now includes its `capabilities` object. WebUI GGUF tab can show capability badges.

#### 4.1.5 New endpoint: GET /api/v1/models/capabilities

**File**: `cmd/cppworker/router.go` + `internal/balancer/`

```
GET /api/v1/models/capabilities?model=qwen3-4b
→ {
    "model": "qwen3-4b",
    "capabilities": {
        "reasoning": false,
        "vision": false,
        "tools": true,
        "embeddings": false,
        "maxContext": 32768,
        "parallel": 1
    }
}
```

This is a "lightweight" check clients can do before sending tools/images to a model.

### 4.2 P0: Explicit Cancel API

**Goal**: User-cancel propagates to backend in <10ms with proper telemetry.

#### 4.2.1 New cppworker endpoint: POST /api/cancel

**File**: `cmd/cppworker/handlers_inference.go` (new) + `cmd/cppworker/router.go`

```go
// POST /api/cancel
// Body: {"request_id": "req1234"}  or  {"session_id": "..."}
type CancelRequest struct {
    RequestID string `json:"request_id"`
    SessionID string `json:"session_id"`
}

type CancelResponse struct {
    Cancelled bool   `json:"cancelled"`
    Reason    string `json:"reason,omitempty"`
}

func handleCancel(w http.ResponseWriter, r *http.Request) {
    var req CancelRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        writeError(w, 400, "invalid body: " + err.Error())
        return
    }
    
    if req.RequestID != "" {
        if cancelled := backend.CancelByRequestID(req.RequestID); cancelled {
            writeJSON(w, 200, CancelResponse{Cancelled: true, Reason: "request_id"})
            return
        }
    }
    if req.SessionID != "" {
        if cancelled := backend.CancelBySessionID(req.SessionID); cancelled {
            writeJSON(w, 200, CancelResponse{Cancelled: true, Reason: "session_id"})
            return
        }
    }
    writeJSON(w, 404, CancelResponse{Cancelled: false, Reason: "not found"})
}
```

#### 4.2.2 Track active generations by request_id in cppworker

**File**: New `cmd/cppworker/active_generations.go`

```go
type activeGeneration struct {
    RequestID string
    SessionID string
    Model     string
    StartedAt time.Time
    Cancel    context.CancelFunc
    Done      chan struct{}
}

type ActiveGenerations struct {
    mu       sync.Mutex
    byReqID  map[string]*activeGeneration
    bySessID map[string]*activeGeneration
}

func (a *ActiveGenerations) Register(reqID, sessID, model string) (context.Context, context.CancelFunc) {
    ctx, cancel := context.WithCancel(context.Background())
    a.mu.Lock()
    defer a.mu.Unlock()
    gen := &activeGeneration{
        RequestID: reqID, SessionID: sessID, Model: model,
        StartedAt: time.Now(), Cancel: cancel, Done: make(chan struct{}),
    }
    a.byReqID[reqID] = gen
    if sessID != "" { a.bySessID[sessID] = gen }
    return ctx, cancel
}

func (a *ActiveGenerations) Unregister(reqID string) {
    a.mu.Lock()
    defer a.mu.Unlock()
    if gen, ok := a.byReqID[reqID]; ok {
        gen.Cancel()
        close(gen.Done)
        delete(a.byReqID, reqID)
        if gen.SessionID != "" { delete(a.bySessID, gen.SessionID) }
    }
}

func (a *ActiveGenerations) CancelByRequestID(reqID string) bool {
    a.mu.Lock()
    defer a.mu.Unlock()
    if gen, ok := a.byReqID[reqID]; ok {
        gen.Cancel()
        return true
    }
    return false
}
```

#### 4.2.3 Wire into chat/generate handlers

In `writeChatStreamResponse` and `writeGenerateStreamResponse`:
- Before generation: call `activeGens.Register(reqID, sessID, modelName) → ctx, cancel`
- Use `ctx` (not `r.Context()`) for callback's ctx.Done() check
- After generation (defer): call `activeGens.Unregister(reqID)`

This way, when balancer calls `/api/cancel` with the request_id, we immediately cancel the context, the callback returns false on next token, llama.cpp stops.

#### 4.2.4 Balancer calls /api/cancel on client disconnect

**File**: `internal/balancer/proxy_request.go` (modify streaming loop)

```go
case <-clientCtx.Done():
    // 1. Try fast cancel via API (Round 18)
    if requestID := getRequestID(r); requestID != "" {
        cancelURL := targetURL + "/api/cancel"
        body := fmt.Sprintf(`{"request_id":%q}`, requestID)
        cancelCtx, cancelCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
        defer cancelCancel()
        cancelReq, _ := http.NewRequestWithContext(cancelCtx, "POST", cancelURL, strings.NewReader(body))
        cancelReq.Header.Set("Content-Type", "application/json")
        if resp, err := httpClient.Do(cancelReq); err == nil {
            resp.Body.Close()
            logger.Get().Infow("proxyRequestOpenAIStreaming: explicit cancel sent to backend",
                "backend", backendID, "request_id", requestID, "status", resp.StatusCode)
        } else {
            logger.Get().Warnw("proxyRequestOpenAIStreaming: explicit cancel failed, falling back to TCP close",
                "backend", backendID, "error", err)
        }
    }
    // 2. Fall back to TCP close (existing behavior)
    return nil
```

This is a non-blocking call (500ms timeout) so it doesn't slow down the balancer. If the cancel API works, the model stops within ms. If it fails, the TCP close still eventually cancels.

#### 4.2.5 Add cancel metrics

In `ActiveGenerations`, count cancellations by reason:
- `cancel_by_request_id_total`
- `cancel_by_session_id_total`
- `cancel_by_tcp_close_total` (fallback)

Expose via `GET /api/v1/balancer/metrics` and per-backend metrics.

### 4.3 P0: Per-user Parallel Session Tracking

**Goal**: Two users on same client don't collide. Per-user count visible.

#### 4.3.1 Add `UserID` extraction

**File**: `internal/balancer/client.go` (modify `getSessionIDWithModel`)

```go
// Extract user identifier for multi-user OpenWebUI/Cline
func (p *Proxy) getUserID(r *http.Request) string {
    // OpenWebUI multi-user: Authorization header (Bearer <jwt>)
    if auth := r.Header.Get("Authorization"); auth != "" {
        // Hash the token (not the raw token) for privacy
        h := sha256.Sum256([]byte(auth))
        return "auth:" + hex.EncodeToString(h[:8])
    }
    // Cline: X-User-ID header (if set)
    if uid := r.Header.Get("X-User-ID"); uid != "" {
        return "user:" + uid
    }
    // X-Client-ID (workspace ID in Cline)
    if cid := r.Header.Get("X-Client-ID"); cid != "" {
        return "client:" + cid
    }
    // Fallback: IP + fingerprint
    return "fp:" + p.getClientFingerprint(r)
}
```

#### 4.3.2 Update session ID formula

```go
func (p *Proxy) getSessionIDWithModel(r *http.Request, clientName, model string) string {
    userID := p.getUserID(r)
    endpointType := p.getOllamaEndpointType(r.URL.Path)
    // User + model + endpoint = distinct session per user
    return userID + "::" + model + "::" + endpointType
}
```

This is a **breaking change** for existing sessions but the user explicitly asked for it. Document in CHANGELOG.

#### 4.3.3 Per-user stream count tracking

**File**: `internal/balancer/session_manager.go`

```go
type SessionManager struct {
    sessions map[string]*types.Session
    mu       sync.RWMutex
    // ... existing fields ...
    
    // Per-user stream counter (Round 18)
    activeByUserID   map[string]int
    activeByModel    map[string]int  // model name → count
    activeByClient   map[string]int  // client name → count
}
```

In `SetStreamActive` and `Clear`:
```go
func (sm *SessionManager) SetStreamActive(id string, active bool) {
    // ... existing logic ...
    sm.mu.Lock()
    defer sm.mu.Unlock()
    
    if active {
        sm.activeByUserID[session.UserID]++
        sm.activeByModel[session.Model]++
        sm.activeByClient[session.ClientName]++
    } else {
        if count := sm.activeByUserID[session.UserID]; count > 0 { sm.activeByUserID[session.UserID]-- }
        if count := sm.activeByModel[session.Model]; count > 0 { sm.activeByModel[session.Model]-- }
        if count := sm.activeByClient[session.ClientName]; count > 0 { sm.activeByClient[session.ClientName]-- }
    }
}
```

#### 4.3.4 Use per-user count in admission control

**File**: `internal/balancer/proxy.go` (modify `tryAcquireSlot` flow)

In ServeHTTP, after determining session/backend:
```go
userActive := p.sessionMgr.GetActiveStreamCountByUser(session.UserID)
modelMaxParallel := p.getModelMaxParallel(model)  // from /api/models capabilities

if userActive >= modelMaxParallel {
    logger.Get().Infow("user at parallel limit, queueing",
        "user_id", session.UserID, "active", userActive, "max", modelMaxParallel)
    if !p.queueRequest(w, r, model) {
        http.Error(w, "Service unavailable - per-user parallel limit reached", http.StatusServiceUnavailable)
    }
    return
}
```

This prevents one user from monopolizing all parallel slots.

### 4.4 P1: Per-model Live Metrics

**Goal**: Real-time visibility into model utilization.

#### 4.4.1 cppworker: GET /api/infer/active

**File**: New `cmd/cppworker/handlers_inference.go`

```go
func handleInferActive(w http.ResponseWriter, r *http.Request) {
    snapshot := backend.GetActiveGenerations()
    writeJSON(w, 200, snapshot)
}

type InferActiveSnapshot struct {
    Total     int                    `json:"total"`
    Generations []InferActiveEntry   `json:"generations"`
}

type InferActiveEntry struct {
    RequestID   string `json:"request_id"`
    SessionID   string `json:"session_id,omitempty"`
    Model       string `json:"model"`
    StartedAt   string `json:"started_at"`
    ElapsedMs   int64  `json:"elapsed_ms"`
    PromptTokens int   `json:"prompt_tokens,omitempty"`
    GeneratedTokens int `json:"generated_tokens,omitempty"`
}
```

#### 4.4.2 cppworker: GET /api/infer/metrics

```go
type InferMetricsSnapshot struct {
    TotalRequests     int64            `json:"total_requests"`
    TotalTokensGen    int64            `json:"total_tokens_generated"`
    AvgTokensPerSec   float64          `json:"avg_tokens_per_sec"`
    ActiveGenerations int              `json:"active_generations"`
    ByModel           map[string]ModelInferStats `json:"by_model"`
}

type ModelInferStats struct {
    Requests       int64   `json:"requests"`
    TokensGen      int64   `json:"tokens_generated"`
    TokensPerSec   float64 `json:"tokens_per_sec"`
    ActiveCount    int     `json:"active_count"`
    LastRequestAt  string  `json:"last_request_at,omitempty"`
}
```

#### 4.4.3 Balancer: aggregate per-model stats

In `llamaCppMetricsPoller`, poll /api/infer/metrics every 5s and merge with /api/models.

Expose via:
- `GET /api/v1/balancer/models` → list of models with stats
- Add to `GET /api/v1/gguf/backends` (each model in LoadedModels gets stats)

#### 4.4.4 Per-user active count in /api/metrics

Add to BalancerMetrics.GetMetrics():
```go
"active_by_user": sessionMgr.GetActiveByUser(),
"active_by_model": sessionMgr.GetActiveByModel(),
"active_by_client": sessionMgr.GetActiveByClient(),
```

---

## 5. Implementation Plan (ordered commits)

### Commit 1: P0.1 - Model Capabilities
- `pkg/types/llama_cpp_metrics.go` — add `ModelCapabilities` struct
- `cmd/cppworker/capabilities.go` (new) — `DetectCapabilities()`
- `cmd/cppworker/handlers_model.go` — populate `Capabilities` in `/api/models` response
- `internal/balancer/llamacpp_metrics_poller.go` — parse `Capabilities` from poll
- `internal/balancer/llamacpp_transport.go` — set `X-Model-Capabilities` header on response
- `cmd/cppworker/capabilities_test.go` (new) — unit tests for detection logic
- Estimated: 200-300 lines, 4-6 files

### Commit 2: P0.2 - Explicit Cancel API
- `cmd/cppworker/active_generations.go` (new) — `ActiveGenerations` registry
- `cmd/cppworker/handlers_chat.go` + `handlers_generate.go` + `handlers_openai.go` — register/unregister active gens
- `cmd/cppworker/handlers_inference.go` — new `handleCancel` endpoint
- `cmd/cppworker/router.go` — new route `POST /api/cancel`
- `internal/balancer/proxy_request.go` — call /api/cancel on client disconnect
- `internal/balancer/cancel_metrics.go` (new) — counter
- Tests: unit + integration
- Estimated: 300-500 lines, 8-10 files

### Commit 3: P0.3 - Per-user Session Tracking
- `internal/balancer/client.go` — `getUserID()` extraction
- `internal/balancer/session_manager.go` — per-user/model/client counters
- `internal/balancer/proxy.go` — admission control with per-user parallel limit
- `pkg/types/session.go` — add `UserID` field
- `internal/balancer/session_handler.go` — expose counts
- Estimated: 150-250 lines, 4-5 files

### Commit 4: P1.4 - Per-model Live Metrics
- `cmd/cppworker/handlers_inference.go` — `handleInferActive` + `handleInferMetrics`
- `cmd/cppworker/router.go` — new routes
- `internal/balancer/llamacpp_metrics_poller.go` — poll new endpoints
- `internal/balancer/metrics.go` — include in GetMetrics
- `internal/balancer/llamacpp_handlers_readonly.go` — new /api/v1/balancer/models
- Tests
- Estimated: 200-300 lines, 5-7 files

### Commit 5: Verification Suite
- `tests/verify_bundled/test_capabilities.py` (new)
- `tests/verify_bundled/test_cancel.py` (new)
- `tests/verify_bundled/test_per_user.py` (new)
- `tests/verify_bundled/test_per_model_metrics.py` (new)
- Update `run_all.py` to include new tests
- Estimated: 400-500 lines, 5 files

### Commit 6: Documentation
- `CHANGELOG.md` — v0.5.3 entry
- `README.md` — mention new capabilities + cancel
- `docs/architecture.md` (new if not exists) — balancer architecture diagram

---

## 6. Verification Tests (must all pass before v0.5.3)

### C1: Capabilities
- T1: Load qwen3-instruct (no reasoning) → response has `X-Model-Capabilities: tools` (no reasoning)
- T2: Load qwen3-thinking → response has `X-Model-Capabilities: reasoning,tools`
- T3: Load model with mmproj in same dir → `X-Model-Capabilities: vision,...`
- T4: `GET /api/v1/models/capabilities?model=qwen3-4b` returns expected JSON
- T5: Embedding model (bge-small) → `embeddings:true`, others:false

### C2: Cancel
- T1: Start streaming, client disconnects after 100ms → backend logs "cancelled by request_id" within 1s
- T2: Backend `/api/cancel` with non-existent request_id → 404
- T3: Concurrent: 2 streams, cancel one, other continues
- T4: Cancel + telemetry: cancellation counter increments
- T5: Cancel during BatchedScheduler batched_decode → next token stops, slot released

### C3: Per-user
- T1: 2 users (different Authorization headers) on OpenWebUI → 2 distinct sessions, 2 parallel generations
- T2: 1 user spams same model with 3 parallel requests → 2 queued (maxParallel=2), 1 admitted
- T3: User disconnects from 1 stream → active count decrements
- T4: Per-user count in /api/metrics matches actual active streams

### C4: Per-model metrics
- T1: Load model, send 1 request, wait → `total_tokens_generated` increases
- T2: Concurrent: 2 requests on same model → `active_count=2` in /api/infer/active
- T3: 2 different models → `by_model` shows separate stats
- T4: WebUI dashboard shows live active count for each model

---

## 7. Breaking Changes (need user approval)

1. **Session ID formula change** (P0.3) — existing session bindings will be lost on upgrade. Recommendation: keep old formula for 1 release with deprecation warning, then switch.
2. **Response headers added** (P0.1) — non-breaking, just new headers.
3. **/api/models response shape extended** (P0.1) — non-breaking, new fields are additive.
4. **/api/cancel new endpoint** (P0.2) — non-breaking, new endpoint.

---

## 8. Out of Scope (for later rounds)

- **Vision image attachment pass-through** — needs llama.cpp mmproj integration in cppworker
- **Code completion / FIM** — needs chat template + special token handling
- **logprobs pass-through** — cppworker doesn't support yet
- **system_fingerprint pass-through** — trivial, but no current use case
- **OpenAI prompt_tokens_details.cached_tokens** — would need cppworker KV cache accounting
- **Multi-GPU model replication** — Round 17 plan, separate work
- **Per-prompt evaluation (load test)** — Round 17 plan, separate work

---

## 9. Risk Assessment

**Low risk**:
- P1.4 metrics (additive, no behavior change)
- P0.1 capability detection (heuristics + header additions)

**Medium risk**:
- P0.2 cancel API (race conditions, double-cancel)
- P0.3 per-user tracking (breaking change for session IDs)

**Mitigation**:
- Cancel: make new API opt-in (env var `CPPWORKER_ENABLE_CANCEL_API=1`), keep TCP fallback always
- Per-user: feature flag for new session ID formula, can be reverted in 5min
- Heavy unit tests + integration tests + live verification before each tag

---

## 10. Open Questions for User

1. **Session ID break**: Acceptable to lose existing session bindings on upgrade? (Default: yes, but document)
2. **MaxParallel default**: If model capabilities don't include `parallel`, should we use backend's `MaxConcurrentReqs` as fallback? (Default: yes)
3. **Cancel API auth**: Should `/api/cancel` require the same X-API-Token as other admin endpoints? (Default: yes, to prevent DoS)
4. **Capability header in response vs separate endpoint**: Both? Or just one? (Default: header for backward compat + endpoint for explicit query)
5. **Per-user limit**: Default to `min(parallel, 2)` per user? (Default: `parallel` — let users max out their own capacity)

---

**Document version**: 1.0
**Last updated**: 2026-08-03
**Status**: Awaiting user review and approval
