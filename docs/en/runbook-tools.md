# Runbook: Debugging tools/tool_calls request resets

**Version:** 2026-06-22
**Purpose:** step-by-step guide for finding the reasons why OpenWebUI / Cline / Roo Code
receive a "reset" or empty response on requests with `tools[]` to OllamaLegion (cppworker/llama.cpp).

---

## 1. Symptoms

The client reports one of the following (often all at once):

- `one source was used, but the response itself is missing`
- HTTP 5xx / 413 on every other iteration of a dialog with tools
- `data: [DONE]` without `tool_calls` (the model "thought" but did not call a tool)
- an infinite "reload loop": the model is unloaded and loaded again on every request
- HTTP 503 `model is loading` right after the first request

---

## 2. Where to look (top-down)

### 2.1 Client level

What the client sends → what it receives:

```bash
# PowerShell
curl -X POST http://localhost:18080/v1/chat/completions `
  -H "Content-Type: application/json" `
  -d (Get-Content test_toolcall.json -Raw) `
  -o response.json

# View the raw response
Get-Content response.json
```

Enable `--verbose` (`-v` in curl) to see the headers: presence of `X-Cpp-Ctx` from the balancer
and `Content-Type: text/event-stream` (SSE) vs `application/x-ndjson` (Ollama format).

**Key fields in the request:**

| Field | Where | What to check |
|---|---|---|
| `tools[]` | body | Must be present (OpenWebUI always sends it when tools are enabled) |
| `model` | body | Matches what exists on the backend (qwen2.5:7b-instruct-q4_K_M, etc.) |
| `stream` | body | `true` for OpenWebUI — otherwise the balancer proxies via a different path |
| `options.num_ctx` | body | If the client sets it explicitly (e.g. 16384), the balancer clamps it via the `X-Cpp-Ctx` header |
| `options.num_predict` / `max_tokens` | body | 0 = invalid; 31744 + 6887 prompt > 32768 n_ctx → `code 3` |

### 2.2 Load balancer level

Verify that the request actually reached the load balancer:

```bash
curl -H 'X-API-Token: <token>' http://localhost:18081/api/v1/health
curl -H 'X-API-Token: <token>' http://localhost:18081/api/v1/backends
```

Load balancer logs (zap JSON to stdout):

```powershell
docker logs -f deployments-loadbalancer-1 2>&1 | Select-String "tools|chat_id|stream|/v1/chat"
```

**What to look for:**

- `parsed request: model=... stream=...` — the balancer parsed the body.
- `[BALANCER → BACKEND] POST /api/chat` or `/v1/chat/completions` — the request was sent to the backend.
- `proxyRequestOpenAIStreaming: heartbeat write failed` — the client closed the connection due to timeout (often during a reload loop).

### 2.3 cppworker level (main source of the issue)

Enable verbose mode:

```yaml
# deployments/.env.bundled
CPPWORKER_VERBOSE=true
```

or:

```powershell
$env:CPPWORKER_VERBOSE = "true"
```

Restart cppworker and repeat the request.

**Key logs to find the cause of a reset:**

| Log message | What it means | Action |
|---|---|---|
| `clamping n_predict to fit n_ctx` | `n_predict` was reduced via `clampNPredictToFitContext`. Fields: `actual_prompt_tokens`, `requested_n_predict`, `clamped_n_predict`, `n_ctx` | If `actual_prompt_tokens` is large (>4096 with n_ctx=8192) — increase the n_ctx in the model profile |
| `applyCppCtxHeader: clamping body num_ctx to balancer header limit` | The balancer reduced n_ctx | Check the `X-Cpp-Ctx` header in the request from the balancer |
| `applyCppCtxHeader: replacing default n_predict with n_ctx-reserve` | n_predict was replaced with `n_ctx - reserve`. Fields: `has_tools`, `prompt_reserve`, `reference_default` | If `has_tools=false` for an actual tools request — the handler did not pass the option |
| `RAM fallback: reload disabled for tools-request` | **Main source of "reset"** — reload for tools is disabled (`inference.go:491`) | Client will receive HTTP 413. Resolution: reduce history/tools or increase n_ctx in the profile |
| `RAM fallback: cycle limit reached, refusing reload` | `ramFallbackMaxAttempts=3` exceeded within 60 sec. Fields: `attempts`, `elapsed`, `max_attempts` | This is the "infinite reload loop". Resolution: restart cppworker or wait out the window |
| `RAM fallback: reloading model with larger n_ctx` | Model reload started. Fields: `old_n_ctx`, `new_n_ctx`, `old_gpu_layers`, `new_gpu_layers`, `use_mmap` | During reload (10-30 sec) all requests to the model wait or receive 503 `model is loading` |
| `RAM fallback: model reloaded successfully` | Successful reload | After this, requests should go through normally |
| `bridge code 3` or HTTP 413 + `prompt too long` | C-bridge returned `ErrPromptTooLong`. Sum `prompt + n_predict + 1 > n_ctx` | Reduce prompt or n_predict |

---

## 3. Reproduction on a specific machine

### 3.1 Quick reproduction (bundled stack)

```powershell
# Start the bundled stack (cppworker-gpu + balancer + webui)
.\scripts\start-bundled.ps1 -Rebuild

# Reproduce the issue (test_toolcall.json contains a request with a single tool `search`)
curl -X POST http://localhost:18080/v1/chat/completions `
  -H "Content-Type: application/json" `
  -d (Get-Content test_toolcall.json -Raw)
```

If we get an empty response or 413 — the issue is reproduced.

### 3.2 Comparison with a "working" ollama on the same machine

```powershell
# 1. Start ollama directly on port 11435
$env:OLLAMA_HOST = "127.0.0.1:11435"
ollama serve

# 2. Register this ollama as a backend in the load balancer
curl -X POST http://localhost:18081/api/v1/backends `
  -H "X-API-Token: <token>" -H "Content-Type: application/json" `
  -d '{"id":"ollama-native","host":"127.0.0.1","ollama_port":11435,"type":"ollama","weight":1,"max_concurrent_reqs":10}'

# 3. Run the same test, targeting ollama-native
curl -X POST http://localhost:18080/api/chat `
  -H "Content-Type: application/json" `
  -d '{"model":"qwen2.5:7b-instruct-q4_K_M","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"search","description":"Search","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}],"stream":false}'
```

**If ollama-native works but cppworker does not:** the issue is in the cppworker logic (clamp/fallback/Hermes detection).

**If both do not work:** the issue is in the load balancer or in the request format (for example, Ollama native does not understand `tools[]` without `options.tools` — conversion is required).

### 3.3 Stub mode for local debugging (without GPU)

```powershell
# Build the stub binary
go build -tags llama_stub -o cppworker-stub.exe .\cmd\cppworker

# Run the stub on port 18092
.\cppworker-stub.exe --port 18092 --models-dir ./models

# In a separate terminal — our bundled balancer should be running on 18081
# Register the stub:
curl -X POST http://localhost:18081/api/v1/backends `
  -H "X-API-Token: <token>" -H "Content-Type: application/json" `
  -d '{"id":"cppworker-stub","host":"127.0.0.1","ollama_port":18092,"cppworker_port":18092,"type":"llama_cpp","engine":"llama_cpp","weight":1,"max_concurrent_reqs":10}'
```

Stub mode is useful for verifying transport logic (SSE streaming, NDJSON parsing, tools fallback),
but **NOT** for verifying real n_ctx overflow (because the stub has no real tokenizer).

### 3.4 Running existing diagnostic tests

```powershell
# Baseline for verifying transport proxying of tools
go test ./tests/ -run "TestDebugOpenWebUI_ToolCalls" -tags llama_stub -count=1 -v

# New tests for verifying inference logic (clamp/fallback/Hermes)
go test ./tests/ -run "TestCppWorker_ToolsRequest" -tags llama_stub -count=1 -v
```

---

## 4. Specific reset scenarios and their causes

### 4.1 Scenario A: HTTP 413 on the very first request with tools

**Symptom:** the client immediately receives 413; the model does not have time to generate anything.

**Root cause:** the sum `prompt_tokens + NPredict + 1 > n_ctx` → C-bridge returns `code 3` →
`clampNPredictToFitContext` truncates NPredict down to `minNPredictClamp=512`, but still not enough →
`tryRamFallbackReload` refuses to reload for tools (`hasTools=true`) →
returns `*ReloadDisabledForToolsError` → the handler translates this into HTTP 413.

**Diagnostics:**

1. Log `clamping n_predict to fit n_ctx` with `clamped_n_predict < requested_n_predict`.
2. Log `RAM fallback: reload disabled for tools-request`.
3. Log `bridge code 3` (if it appears before the fallback).

**Resolution:**

- Reduce the number of tools / length of the system prompt in OpenWebUI.
- Increase `n_ctx` in the model profile (via WebUI → Manage Models).
- Set `CPPWORKER_RAM_FALLBACK_N_CTX=true` and `CPPWORKER_RAM_FALLBACK_MAX_N_CTX=65536` — this will allow reload for tools (but potentially with a slow mmap fallback).

### 4.2 Scenario B: first request OK, second one is an "empty response"

**Symptom:** the first iteration of a dialog with tools passes normally; the second one (with history) is an empty NDJSON chunk.

**Root cause:** on the second iteration, the prompt includes the full history + tool definitions + tool results.
In total `history_tokens + tool_defs + NPredict + 1 > n_ctx`. `clampNPredictToFitContext` truncates NPredict
down to 512, and even with `minNPredictClamp` the model cannot generate anything meaningful — the final chunk
with `done:true` arrives with empty `content` and without `tool_calls`. The client sees "the model used one
source, but the response is missing".

**Diagnostics:**

1. Log `clamping n_predict to fit n_ctx` with `actual_prompt_tokens` >> 2048.
2. Log `applyCppCtxHeader: replacing default n_predict with n_ctx-reserve` with `has_tools=true` and
   `clamped_n_predict < 1024`.
3. Final NDJSON chunk: `message.content=""`, `message.tool_calls=null`.

**Resolution:**

- Enable "Truncate history" (Sliding Window) in OpenWebUI.
- Reduce `MAX_HISTORY_TOKENS` or `MAX_TOOLS` in the client settings.
- Increase `n_ctx` to 16384-32768.

### 4.3 Scenario C: infinite reload loop

**Symptom:** cppworker logs constantly show `RAM fallback: reloading model with larger n_ctx` and
`RAM fallback: model unloaded` → the client receives 503 `model is loading` or timeouts.

**Root cause:** `ramFallbackMaxAttempts=3` within 60 sec is exceeded, but every iteration of the dialog
still triggers an overflow → cycle limit → `ReloadLoopLimitError` → HTTP 413. This looks like
"the model is being reset" (because the reload operations are indeed visible in the logs).

**Diagnostics:**

1. Log `RAM fallback: cycle limit reached, refusing reload` with `attempts=3`, `elapsed~10s`.
2. Log `RAM fallback: reloading model with larger n_ctx` repeating 3+ times within 60 sec.
3. The client sees HTTP 413 with the message `reload limit reached... reduce tools/prompt`.

**Resolution:**

- Immediate: restart cppworker (`docker restart deployments-cppworker-gpu-1`) to reset the
  `ramFallbackAttempts` counter.
- Long-term: eliminate the root cause of the overflow (see 4.1 / 4.2).

### 4.4 Scenario D: tool_call in content is not extracted (Hermes/Qwen)

**Symptom:** cppworker streams SSE with `delta.content="<tool_call>{...}</tool_call>"` (Hermes-style),
but the client (OpenWebUI via the balancer) receives the final NDJSON without `message.tool_calls`.

**Root cause:** cppworker returns the tool_call in content (because the model's chat template is
Hermes/Qwen). The balancer, when translating SSE→NDJSON, must call `detectAndExtractToolCallsFromContent`,
but either does not call it, or the parser does not find the pattern.

**Diagnostics:**

1. cppworker log: the final chunk contains `choices[0].delta.content="<tool_call>..."`, without `tool_calls`.
2. Load balancer log: `tool_calls found: false` or similar.
3. In the client's raw response: `message.tool_calls=null`, but `message.content="<tool_call>..."`.

**Resolution:**

- This is a known issue (see `tests/openwebui_tool_calls_debug_test.go:684-911`, scenarios E/E2).
- Verify that in `cmd/cppworker/tool_calls.go` the parser `parseToolCallsFromOutput` /
  `extractHermesToolCalls` covers the format of the model being used.
- If the model emits `[TOOL_CALLS][...]` — check `extractMistralToolCalls`.
- If `<|python_tag|>{...}` — `extractLlamaPythonTagCalls`.

### 4.5 Scenario E: "reset" with no visible cause in the logs

**Symptom:** the client receives HTTP 5xx or an empty response; in the cppworker logs there is no fallback,
no clamp, no cycle limit — but the balancer answers "reset by peer".

**Root cause:** timeout on the balancer side. `WriteTimeout` of cppworker
(default 30 minutes, but can be overridden via `CPPWORKER_WRITE_TIMEOUT` or `--write-timeout`),
or `RequestTimeout` of the balancer (`Balancing.RequestTimeout=30` by default).

**Diagnostics:**

1. Time from request start to the error in the balancer logs.
2. cppworker log: `backend.GenerateStream` finished in X seconds; if X > `RequestTimeout` —
   the balancer closed the connection.
3. cppworker log: `write: broken pipe` or `connection reset by peer` — the client closed the connection earlier.

**Resolution:**

- Increase `Balancing.RequestTimeout` (in `config/config.json`) to 300-600 seconds.
- Increase cppworker's `WriteTimeout`.
- Reduce `max_tokens` / `num_predict` in the request — long generation may exceed the timeout.

### 4.6 Scenario F: prompt > n_ctx even with reload (Cline 55K tokens)

**Symptom:** the client (Cline/OpenWebUI) sends a prompt significantly larger than the model's current
`n_ctx` — for example, Cline sends 55111 tokens + `n_predict=512` = 55624,
while the model is loaded with `n_ctx=32768`, and VRAM allows at most
`max_vram_n_ctx=32719` (or any value ≤ required). The user sees:

```
[OLLAMA] Ollama stream processing error: stream inference failed with code 3:
prompt too long for n_ctx: prompt_tokens=55111 + n_predict=512 + 1 = 55624
> n_ctx=32768 (model loaded with n_ctx=32768, request asked for n_ctx=32768).
Reduce prompt, set smaller max_tokens, or save a model profile with bigger
n_ctx and reload the model (current_n_ctx=32768, required_n_ctx=55624,
max_vram_n_ctx=32719): prompt + n_predict exceeds n_ctx
```

**Root cause:** even with `AutoReloadNCtx=true` and `AutoTuneNCtx` enabled,
the balancer CANNOT reload the model with `n_ctx > MaxVRAMNCtx * safety_factor`
(default 0.85) — VRAM physically cannot hold that much KV-cache. Preflight
in this case returns HTTP 413 with an actionable suggestion.

**Resolution (in increasing order of invasiveness):**

1. **Save a model profile with a larger `n_ctx`** and apply it, IF
   `MaxVRAMNCtx` supports the required size in principle:
   ```bash
   # Find out the current VRAM limit
   curl http://cppworker:18092/api/v1/cppworker/config | jq .runtime
   
   # PUT profile with n_ctx=65536 (if VRAM=80GB and model_max supports it)
   curl -X PUT http://balancer:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M \
     -H 'X-API-Token: <token>' \
     -H 'Content-Type: application/json' \
     -d '{"contextLength": 65536, "batchSize": 512, "numGpuLayers": -1, "flashAttn": true}'
   
   # Apply (save + reload on all backends)
   curl -X POST http://balancer:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M/apply \
     -H 'X-API-Token: <token>'
   ```

2. **Reduce the prompt or `n_predict`** in the client settings. Cline:
   `Settings → API Configuration → Max Tokens`. OpenWebUI:
   `Workspace → Models → Parameters → Max Tokens`.

3. **Switch to a GPU with more VRAM** (A100 80GB, H100, etc.) — the only
   way to bypass the physical limit for very large prompts.

**Preflight (new behavior, default since 2026-06-23):**

- The balancer estimates the prompt size BEFORE sending the request (`chars/4 + n_predict + 1`).
- If `estimated > current_n_ctx` AND `≤ MaxVRAMNCtx * 0.85` AND `≤ model_max_context`
  → the balancer **itself** reloads the model with the target `n_ctx` via `POST /api/models/reload`,
  waits for completion, and sends the request. One round-trip, without code 3.
- If `> MaxVRAMNCtx * 0.85` → HTTP 413 with JSON `{error, reason, required_n_ctx,
  current_n_ctx, max_vram_n_ctx, model_max_context, suggestion, profile_endpoint}`.
- Log markers: `preflight: triggering reload`, `preflight: reload succeeded`,
  `preflight: reject (VRAM/model_max exhausted)`.

**Configuration flags:**

- `LB_NCTX_RELOAD_ENABLED=true` (default true) — enables auto-reload.
- `LB_NCTX_RELOAD_ALLOW_TOOLS=true` (default true) — allows reload for tools requests.
- `LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR=0.85` — fraction of MaxVRAMNCtx available for KV-cache.
- `CPPWORKER_RAM_FALLBACK_ALLOW_TOOLS=true` (default true) — on the cppworker side.
- To revert to the old behavior (without auto-reload for tools):
  `LB_NCTX_RELOAD_ENABLED=false` OR `LB_NCTX_RELOAD_ALLOW_TOOLS=false`.

### 4.7 Scenario G: EOF during preflight n_ctx reload (cppworker drops the connection) — FIXED 2026-06-24

**Symptom:** OpenWebUI / curl receive **empty response / EOF / connection reset** on the very first
request with `options.num_ctx` different from the default (e.g. `num_ctx=16384`, while the model is loaded
with `n_ctx=8196`). In cppworker logs — `UnloadModel → LoadModelWithOpts` reload. In balancer
logs — `preflight: triggering reload` + `queryCppWorkerModels: EOF`.

**Root cause (FIXED):**

cppworker in `handleReloadModel` called `backend.UnloadModel()` → `backend.LoadModelWithOpts()`
**without waiting for active inference requests to complete**. At the moment of `UnloadModel`, cppworker
dropped HTTP connections of all current requests → clients received EOF. The balancer,
which was polling `/api/models` via `queryCppWorkerModels` at that moment,
caught a connection reset / EOF and hung in the `concurrent load already in progress, waiting`
polling loop (model state=loading, but polling also failed).

**What was done (Track 1: cppworker graceful reload):**

1. `internal/cppbackend/inflight.go` — new per-model `InFlightCounter` (atomic).
2. `cmd/cppworker/handlers_generate.go:handleGenerate` / `handleOllamaGenerate`,
   `handlers_chat.go:handleChat`, `handlers_openai.go:handleV1ChatCompletions` /
   `handleV1Completions` — each does `Inc(modelName)` on entry and `Dec(modelName)` in defer.
3. `cmd/cppworker/handlers_model.go:handleReloadModel` — **before** `UnloadModel` calls
   `backend.InFlight().WaitZero(modelName, 0)` (no limit; the general watchdog is at the reload level).
4. `internal/cppbackend/backend.go:SetReloadPending/GetReloadPending` — adds
   `reload_pending: {model, startedAt, elapsedMs}` to the JSON of `/api/info` during reload.
5. `internal/balancer/preflight_nctx.go:preflightNCtxReloadIfNeededSync` (Track 2) —
   synchronous version of preflight, polling the `/api/info` heartbeat every 500ms, default timeout 60s
   (`Balancing.PreflightSyncTimeoutMs`, max 180s). On timeout — fallback to
   `503 + Retry-After: 15`.

**What was done (Track 3: balancer EOF retry):**

`internal/balancer/llamacpp_backend_helpers.go:queryCppWorkerModels` now:

- Up to 3 attempts with exponential backoff (100ms, 200ms, 400ms) on EOF/`connection reset`/
  `broken pipe`/bad status/decode error.
- On failure of all attempts — fallback to the `lastKnownModels` cache (TTL 30s, per backend).
- `ensureModelLoadedOnBackend` now uses this retry; polling in the
  `concurrent load already in progress, waiting` loop no longer hangs.

**What was done (Track 4: balancer reload dedup):**

`internal/balancer/nctx_reload_dedup.go` — `reloadDedupRegistry` (per backendID+modelName+targetNCtx).
`executeAsyncReload` (preflight) and `ensureModelLoadedOnBackend` are coordinated:

- `IsReloadPending(backendID, modelName)` — if true, polling for state=loading is replaced
  by `WaitReloadDone(timeout=5min)`.
- `StartReloadIfNotPending` — if a reload with the same target is already running, returns
  the existing entry and does not start a second HTTP request.

**Acceptance criteria for verification:**

1. cppworker `handleReloadModel` **does not call** `UnloadModel` while `InFlight().Get(modelName) > 0`.
2. cppworker `/api/info` shows `reload_pending` while reload is in progress.
3. Balancer `preflightNCtxReloadIfNeededSync` returns 200 OK with the proxied
   response if reload completes within `PreflightSyncTimeoutMs` (default 60s).
4. `queryCppWorkerModels` retries 3 times on EOF and returns the last snapshot from the cache.
5. Concurrent `executeAsyncReload` (preflight) + `ensureModelLoadedOnBackend` —
   only one HTTP reload on cppworker, the second waits via `WaitReloadDone`.

**Configuration:**

- `Balancing.PreflightSyncEnabled` (default `true`) — sync mode (single round-trip).
- `Balancing.PreflightSyncTimeoutMs` (default `60000`, max `180000`) — wait timeout.
- Kill-switch for legacy scripts: `Balancing.PreflightSyncEnabled=false` →
  fallback to async 503 + `Retry-After: 5` (old behavior).

---

## 5. Diagnostician's checklist

1. ☐ `--verbose` enabled (`CPPWORKER_VERBOSE=true`) on cppworker.
2. ☐ Checked `data/state.json` (balancer) — list of backends and their statuses.
3. ☐ Checked balancer logs: `parsed request`, `[BALANCER → BACKEND]`, `heartbeat write failed`.
4. ☐ Checked cppworker logs: `clamping n_predict`, `RAM fallback`, `bridge code N`.
5. ☐ Ran `TestDebugOpenWebUI_ToolCalls_ScenarioA..E2` — whether baseline tests pass.
6. ☐ Ran `TestCppWorker_ToolsRequest_*` (new tests from `cppworker_inference_test.go`).
7. ☐ Comparison with `ollama serve` directly — if ollama works, the issue is in cppworker.
8. ☐ Checked `n_ctx` and `n_predict` in the request (via `curl -v` + `X-Cpp-Ctx` header from the balancer).
9. ☐ Checked the length of `tools[]` and system prompt in OpenWebUI (via WebUI → Workspace → Tools).
10. ☐ Checked cppworker's `WriteTimeout` and the balancer's `RequestTimeout`.
11. ☐ On HTTP 413 with `bridge_info` — checked `current_n_ctx`, `max_vram_n_ctx`, `model_max_context`.
      Resolution in scenario F: save a model profile with a larger `n_ctx` via
      `POST /api/v1/cppworker/model-profiles/{name}/apply` or increase GPU VRAM.
      On `preflight: triggering reload` in the logs — preflight fired, wait for reload to complete.
12. ☐ On `RAM fallback: reload disabled for tools-request (flag off)` — check that
      `CPPWORKER_RAM_FALLBACK_ALLOW_TOOLS=true` (or `LB_NCTX_RELOAD_ALLOW_TOOLS=true`).

---

## 6. What NOT to touch while debugging

- `c/bridge/bridge.c` — low-level C code; changes require a full recompile.
- `c/llama.cpp/` — upstream subtree.
- Files tagged `// DO NOT EDIT` or generated files.

---

## 7. References

- `docs/troubleshooting.md` — general troubleshooting.
- `docs/nctx-troubleshooting.md` — n_ctx specifics.
- `docs/cppworker-routing-fixes-2026-06-07.md` — Phase D.3-fix (X-Cpp-Ctx header).
- `docs/test-report-2026-06-09-cppworker-clamping.md` — Phase D.6/D.8 (clamp n_predict).
- `tests/openwebui_tool_calls_debug_test.go` — diagnostic tests for transport.
- `tests/cppworker_inference_test.go` (new) — diagnostic tests for inference logic.
- `cmd/cppworker/inference.go` — `generateWithRamFallback`, `clampNPredictToFitContext`.
- `cmd/cppworker/nctx_clamp.go` — `ApplyCppCtxHeader`.
- `cmd/cppworker/tool_calls.go` — Hermes/Llama-3/Mistral parsers.
