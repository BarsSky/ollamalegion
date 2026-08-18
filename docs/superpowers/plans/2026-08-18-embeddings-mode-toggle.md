# Dynamic Embeddings Mode Toggle (Option C) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Eliminate the per-decode `embeddings required but some input tokens were not marked as outputs -> overriding` warning (and the wasteful re-mark work it does) by toggling `cparams.embeddings` per-request in the C-bridge. Chat path uses `embeddings=false` (no override, no warning, no per-token logits re-mark), embedding path uses `embeddings=true` (extraction works).

**Why this matters (live evidence):**
- `c/bridge/bridge.c:980` — `ctx_params.embeddings = true;` is hardcoded for all loaded models.
- `c/llama.cpp/src/llama-batch.cpp:131-146` — warning fires per `llama_decode` call when `output_all=true` and not all `batch.logits[i]` are 1. The code path then re-marks all tokens as logits=1 (wasted work for chat).
- For Qwen3.6 22GB on 8GB VRAM (CPU offloaded): 200-token prompt processing gets `200 × n_vocab` logits tensor instead of `1 × n_vocab` (200× extra memory pressure), plus per-decode `t_embd` buffer allocation that's never used. ~5-15% TTFT tax.
- 300+ warning lines per chat request in cppworker logs.
- `c/llama.cpp/src/llama-context.cpp:1153-1160` — `llama_set_embeddings(bool)` is a single bool write (`cparams.embeddings = value;`), no memory impact, no KV cache invalidation. **Free to call per-request.**
- `c/llama.cpp/include/llama.h:998` — `LLAMA_API void llama_set_embeddings(struct llama_context * ctx, bool embeddings);` — public API exists in our fork.

**Architecture:**

1. New C-bridge function `bridge_set_embeddings_mode(ModelHandle m, int mode)` that wraps `llama_set_embeddings(m->context, mode == 1)` and stores the last-set mode in `InferenceModel.embeddings_mode` for observability.
2. New Go wrapper `(*ModelHandle).SetEmbeddingsMode(mode BridgeMode) error`.
3. Chat path: `bridge_batched_decode` and `bridge_infer` set mode 0 (chat) before each `llama_decode` call.
4. Embedding path: `bridge_get_embeddings` sets mode 1 (embedding) before its `llama_decode` call.
5. cppworker `cmd/cppworker/handlers_openai.go` and `handlers_embeddings.go` set the mode before invoking the C-bridge. Or — cleaner — set the mode inside the C-bridge wrappers themselves, so Go callers can't forget.

**Tech Stack:** C11 (c-bridge), Go 1.24 (c-bridge wrappers, cppworker), existing build (cmake + go build + docker buildx).

## Global Constraints

- **Backward compat**: `bridge_set_embeddings_mode` is a NEW function. Existing `bridge_load_model` keeps `ctx_params.embeddings = true` (so the context is still "embedding-capable" — we just toggle off for chat). Embedding endpoint still works.
- **Thread safety**: Chat and embedding endpoints both acquire `instance.mu` (RWMutex) for the entire call. `llama_set_embeddings` is a single bool write inside the C++ `cparams` struct, safe under the mutex. No additional synchronization needed.
- **No new env vars, no config changes**. Pure code fix.
- **No VRAM cost**: zero extra bytes. Single context, single model load.
- **No double model load**: unlike Option B (two contexts), we keep the single ModelHandle.
- **Tests**: C unit test for the toggle, plus live integration test (warning count in logs after chat call should be 0).

---

## File Structure

### Modified files

- `c/bridge/bridge.h` — declare `bridge_set_embeddings_mode()` and `BridgeMode` enum
- `c/bridge/bridge.c` — implement `bridge_set_embeddings_mode`; call it from `bridge_batched_decode` (set mode 0) and `bridge_get_embeddings` (set mode 1)
- `c/bridge/bridge_internal.h` — add `embeddings_mode` field to `InferenceModel` struct
- `c/bridge/bridge.go` — add `BridgeMode` type + `SetEmbeddingsMode(mode BridgeMode) error` method on `*ModelHandle`
- `c/bridge/bridge_stub.go` — stub `SetEmbeddingsMode` for non-CGO builds (test runs)
- `cmd/cppworker/handlers_openai.go` — call `m.SetEmbeddingsMode(bridge.ModeChat)` before BatchedDecode (optional — only if we don't put it in C-bridge)
- `cmd/cppworker/handlers_embeddings.go` — call `m.SetEmbeddingsMode(bridge.ModeEmbedding)` before GetEmbeddings (optional)

### New files

- `c/bridge/tests/test_embeddings_mode.c` — C unit test (assertion-based) for set/unset/toggle behavior
- `tests/cppworker_embeddings_mode_e2e.py` — live e2e test: send a chat request, scrape cppworker logs, assert zero "embeddings required" warnings in last 30s of log

---

## Task 1: C-bridge — add embeddings_mode tracking + setter

**Files:**
- Modify: `c/bridge/bridge_internal.h` — add `embeddings_mode` to `InferenceModel`
- Modify: `c/bridge/bridge.h` — declare `bridge_set_embeddings_mode()`
- Modify: `c/bridge/bridge.c` — implement setter, init embeddings_mode=1 at load

**Interfaces:**
- Consumes: nothing (called from Go or from other C-bridge functions)
- Produces: mutates `im->embeddings_mode` and `im->context->cparams.embeddings`

- [ ] **Step 1: Find InferenceModel struct in bridge_internal.h**

```bash
grep -n "InferenceModel" c/bridge/bridge_internal.h
```

Expected output: a struct definition with fields like `model`, `context`, `vocab`, `ctx_n_ctx`, `ctx_n_batch`, `abort_flag`. Add `int embeddings_mode;` (0=chat, 1=embedding) right after `abort_flag`.

- [ ] **Step 2: Modify InferenceModel struct**

In `c/bridge/bridge_internal.h`, after the existing `abort_flag` field (atomic_int or similar), add:

```c
// Round 39: track embeddings mode for observability (0=chat, 1=embedding).
// Initialized to 1 because bridge_load_model sets ctx_params.embeddings=true.
int embeddings_mode;
```

- [ ] **Step 3: Initialize in bridge_load_model**

In `c/bridge/bridge.c`, in `bridge_load_model` after `im->ctx_n_batch = ctx_params.n_batch;` (or near the end of the function, before the final return), add:

```c
// Round 39: start in embedding mode (matches ctx_params.embeddings=true at load).
// The C-bridge will switch to chat mode (0) before chat decode calls.
im->embeddings_mode = 1;
```

- [ ] **Step 4: Declare bridge_set_embeddings_mode in bridge.h**

In `c/bridge/bridge.h`, near the other ModelHandle functions (e.g., right after `bridge_get_n_vocab` declaration), add:

```c
// Round 39: embeddings mode constants for bridge_set_embeddings_mode.
#define BRIDGE_MODE_CHAT      0
#define BRIDGE_MODE_EMBEDDING 1

// Round 39: dynamically toggle embeddings mode on a loaded model context.
// Used by the C-bridge wrappers before llama_decode calls:
//   - chat path (bridge_batched_decode, bridge_infer) → set BRIDGE_MODE_CHAT
//     so llama.cpp's batch preparation doesn't override logits (no warning, no
//     per-token re-mark work, no extra t_embd memory).
//   - embedding path (bridge_get_embeddings) → set BRIDGE_MODE_EMBEDDING
//     so llama_get_embeddings() returns non-NULL.
//
// Cost: O(1) — just flips ctx->cparams.embeddings bool. No memory ops, no
// KV cache impact, safe to call per-request under the model mutex.
//
// Returns 0 on success, -1 if handle is NULL or context is NULL.
int bridge_set_embeddings_mode(ModelHandle m, int mode);
```

- [ ] **Step 5: Implement bridge_set_embeddings_mode in bridge.c**

In `c/bridge/bridge.c`, add a new function (e.g., right after `bridge_get_n_vocab`):

```c
int bridge_set_embeddings_mode(ModelHandle m, int mode) {
    if (m == NULL) {
        set_error("bridge_set_embeddings_mode: NULL model handle");
        return -1;
    }
    InferenceModel* im = (InferenceModel*)m;
    if (im->context == NULL) {
        set_error("bridge_set_embeddings_mode: NULL context (model not loaded?)");
        return -1;
    }
    // Normalize: anything other than 1 is treated as chat (0). Defensive.
    bool emb_on = (mode == BRIDGE_MODE_EMBEDDING);
    llama_set_embeddings(im->context, emb_on);
    im->embeddings_mode = emb_on ? BRIDGE_MODE_EMBEDDING : BRIDGE_MODE_CHAT;
    return 0;
}
```

- [ ] **Step 6: Build the C-bridge to verify it compiles**

```bash
cd c/build  # (or wherever your cmake build dir is)
cmake --build . --target bridge 2>&1 | tail -30
```

Expected: clean build, no errors. If `bridge_set_embeddings_mode` symbol is exported in `bridge.so` / `bridge.a`, also check:

```bash
nm -D c/build/libbridge.so 2>/dev/null | grep set_embeddings_mode
# or on Windows:
# dumpbin /EXPORTS c\build\bridge.dll | Select-String set_embeddings_mode
```

Expected: symbol present.

---

## Task 2: C-bridge — call set_embeddings_mode in chat path

**Files:**
- Modify: `c/bridge/bridge.c` — `bridge_batched_decode` (and `bridge_infer` / `bridge_infer_stream` if they exist as separate functions)

**Interfaces:**
- Consumes: existing chat path code (no behavior change)
- Produces: side effect of `cparams.embeddings = false` before `llama_decode`

- [ ] **Step 1: Locate chat decode call sites in bridge.c**

```bash
grep -n "llama_decode" c/bridge/bridge.c
```

Expected: at least 2-3 call sites:
- One in `bridge_batched_decode` (main batched chat path)
- One in `bridge_get_embeddings` (we'll set mode=1 here in Task 3)
- Maybe one in `bridge_infer` or `bridge_infer_stream` if they exist as separate functions

- [ ] **Step 2: Add set_embeddings_mode call in bridge_batched_decode**

In `c/bridge/bridge.c`, in `bridge_batched_decode`, RIGHT BEFORE the `llama_decode(im->context, batch)` call (after `llama_batch_init` and population, but before decode), add:

```c
// Round 39: ensure chat mode so llama.cpp doesn't override logits (and
// doesn't emit the "embeddings required" warning). Free op — just flips
// a bool in cparams. Called every chat decode; same handle is serialized
// via instance.mu so no race.
llama_set_embeddings(im->context, false);
im->embeddings_mode = BRIDGE_MODE_CHAT;
```

- [ ] **Step 3: Verify the file compiles**

```bash
cmake --build c/build --target bridge 2>&1 | tail -10
```

Expected: clean build. If there are other chat decode call sites (e.g., `bridge_infer_stream` for token-by-token gen), apply the same pattern — set mode 0 before each `llama_decode`.

- [ ] **Step 4: If bridge_infer or bridge_infer_stream exist as separate functions, apply the same fix**

```bash
grep -n "bridge_infer\|bridge_infer_stream" c/bridge/bridge.c | head -20
```

For each function that calls `llama_decode`, add the same 2-line `llama_set_embeddings(im->context, false)` + `im->embeddings_mode = BRIDGE_MODE_CHAT;` before the decode call.

---

## Task 3: C-bridge — call set_embeddings_mode in embedding path

**Files:**
- Modify: `c/bridge/bridge.c` — `bridge_get_embeddings`

- [ ] **Step 1: Find bridge_get_embeddings**

```bash
grep -n "bridge_get_embeddings" c/bridge/bridge.c
```

Expected: one function definition around line 1831 (per the structure noted in current state).

- [ ] **Step 2: Add set_embeddings_mode call BEFORE the tokenize/decode in bridge_get_embeddings**

In `c/bridge/bridge.c`, in `bridge_get_embeddings`, RIGHT AT THE TOP of the function (after input validation, before tokenization), add:

```c
// Round 39: ensure embedding mode so llama_get_embeddings() returns non-NULL.
// Without this, even if the context was created with embeddings=true, a
// previous chat call may have flipped the mode to false.
llama_set_embeddings(im->context, true);
im->embeddings_mode = BRIDGE_MODE_EMBEDDING;
```

- [ ] **Step 3: Build and verify**

```bash
cmake --build c/build --target bridge 2>&1 | tail -10
```

Expected: clean build.

---

## Task 4: Go bridge — add SetEmbeddingsMode wrapper

**Files:**
- Modify: `c/bridge/bridge.go` — add `BridgeMode` type and `(*ModelHandle).SetEmbeddingsMode`
- Modify: `c/bridge/bridge_stub.go` — stub for non-CGO builds

- [ ] **Step 1: Add BridgeMode type to bridge.go**

In `c/bridge/bridge.go`, after the existing type declarations (e.g., after `ModelConfig` struct), add:

```go
// BridgeMode — embeddings mode flag passed to C-bridge via
// bridge_set_embeddings_mode. Round 39: dynamic toggle per-request so chat
// decode doesn't trigger llama.cpp's "embeddings required" warning.
type BridgeMode int

const (
    ModeChat      BridgeMode = 0
    ModeEmbedding BridgeMode = 1
)
```

- [ ] **Step 2: Add SetEmbeddingsMode method to ModelHandle**

In `c/bridge/bridge.go`, near the other ModelHandle methods (e.g., after `RequestAbort` or `SampleToken`), add:

```go
// SetEmbeddingsMode dynamically toggles the embeddings mode of the loaded
// model's context. Used internally by the C-bridge chat and embedding
// wrappers (chat → ModeChat, embedding → ModeEmbedding), but exposed here
// for external callers (e.g., tests) that want to query or force the mode.
//
// Cost: O(1). Just flips a bool in cparams. Safe to call per-request under
// the model mutex (which the C-bridge already holds during Infer/Embeddings).
//
// Returns an error if the handle is nil or the C call fails (e.g., context
// is NULL because the model was unloaded).
func (m *ModelHandle) SetEmbeddingsMode(mode BridgeMode) error {
    if m == nil || m.ptr == nil {
        return fmt.Errorf("SetEmbeddingsMode: nil model handle")
    }
    rc := C.bridge_set_embeddings_mode(m.ptr, C.int(mode))
    if rc != 0 {
        errStr := ""
        if m.lastErr != nil {
            errStr = *m.lastErr
        }
        return fmt.Errorf("bridge_set_embeddings_mode failed: rc=%d err=%s", rc, errStr)
    }
    return nil
}
```

NOTE: If the existing `ModelHandle` doesn't have a `lastErr` field, use the same error-extraction pattern as `LoadModel` does (check `bridge_get_last_error()` or similar — match what other methods in the file use).

- [ ] **Step 3: Add stub for non-CGO builds**

In `c/bridge/bridge_stub.go`, add:

```go
// SetEmbeddingsMode — stub for non-CGO builds. Does nothing.
func (m *ModelHandle) SetEmbeddingsMode(mode BridgeMode) error {
    return nil
}
```

This keeps `go test` happy without CGO.

- [ ] **Step 4: Verify Go code compiles**

```bash
cd C:\Ollama\ollamalegion
go build ./c/bridge/...
```

Expected: clean build.

---

## Task 5: Unit test (C-side, optional but recommended)

**Files:**
- Create: `c/bridge/tests/test_embeddings_mode.c`

**Why optional:** The C-bridge already has test infrastructure. If a C test runner is wired up, add this. If not, skip to Task 6 and rely on the live e2e test.

- [ ] **Step 1: Create the test file**

```c
// c/bridge/tests/test_embeddings_mode.c
// Round 39: smoke test for bridge_set_embeddings_mode.
//
// Verifies:
//   1. NULL handle → returns -1.
//   2. Valid handle → returns 0, embeddings_mode field is updated.
//   3. Toggle chat → embedding → chat works without crash.
//
// Does NOT load a real model (too expensive for unit test). The C-bridge
// already has test_*.c files; follow their pattern for mock model handles.

#include "bridge.h"
#include <stdio.h>
#include <assert.h>

int main(void) {
    // Test 1: NULL handle.
    int rc = bridge_set_embeddings_mode(NULL, BRIDGE_MODE_CHAT);
    assert(rc == -1);
    printf("test 1 passed: NULL handle rejected\n");

    // Tests 2-3 require a real model load. Skip in unit test, covered by
    // e2e (Task 6).
    printf("test_embeddings_mode: skipped real-model tests (see e2e)\n");
    return 0;
}
```

- [ ] **Step 2: Wire into cmake (if the existing c/bridge/tests/CMakeLists.txt has a pattern)**

```cmake
# In c/bridge/tests/CMakeLists.txt, add:
add_executable(test_embeddings_mode test_embeddings_mode.c)
target_link_libraries(test_embeddings_mode bridge)
add_test(NAME test_embeddings_mode COMMAND test_embeddings_mode)
```

- [ ] **Step 3: Run**

```bash
cmake --build c/build --target test_embeddings_mode
ctest --test-dir c/build -R test_embeddings_mode --output-on-failure
```

Expected: passes.

---

## Task 6: Live e2e test — assert zero "embeddings required" warnings in chat

**Files:**
- Create: `tests/cppworker_embeddings_mode_e2e.py`

- [ ] **Step 1: Create the e2e test**

```python
#!/usr/bin/env python3
"""
Round 39 live e2e: assert that chat requests do NOT emit the
"embeddings required but some input tokens were not marked as outputs"
warning. Embedding requests should still work (and may emit it once at load).
"""
import json
import time
import sys
import urllib.request
import urllib.error
import subprocess
import re

CPPWORKER_HOST = "http://localhost:18091"
BALANCER_HOST  = "http://localhost:18092"
TEST_MODEL     = "Qwen3.6-35B-A3B-UD-Q4_K_M"  # or any loaded model
WARNING_RE     = re.compile(r"embeddings required but some input tokens")


def http(method, host, path, body=None, timeout=120):
    req = urllib.request.Request(host + path, method=method)
    if body is not None:
        req.data = json.dumps(body).encode()
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read())
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")


def count_warnings_since(since_ts):
    """Tail cppworker log, count occurrences of the warning since timestamp."""
    proc = subprocess.run(
        ["docker", "logs", "--since", since_ts, "ol-bundled-cppworker-gpu"],
        capture_output=True, text=True, timeout=30
    )
    return len(WARNING_RE.findall(proc.stdout))


def main():
    # 1. Baseline: capture current cppworker log position.
    since = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime())
    time.sleep(2)  # let any in-flight requests settle

    # 2. Send a chat completion (non-streaming for simplicity).
    print("Sending chat completion...")
    t0 = time.time()
    status, body = http("POST", BALANCER_HOST, "/v1/chat/completions", {
        "model": TEST_MODEL,
        "messages": [{"role": "user", "content": "Reply with the single word OK."}],
        "max_tokens": 10,
        "stream": False
    }, timeout=300)
    ttft = time.time() - t0
    print(f"Chat response: status={status} ttft={ttft:.2f}s")
    print(f"  content={body.get('choices', [{}])[0].get('message', {}).get('content', '')[:50]!r}")
    assert status == 200, f"chat failed: {status} {body}"

    # 3. Wait a moment for logs to flush.
    time.sleep(3)

    # 4. Count warnings emitted AFTER our chat request started.
    warn_count = count_warnings_since(since)
    print(f"Warnings emitted during chat request: {warn_count}")
    assert warn_count == 0, f"expected 0 warnings, got {warn_count} — toggle did not work"

    # 5. Sanity: embedding endpoint still works.
    print("Sending embeddings request...")
    status, body = http("POST", BALANCER_HOST, "/v1/embeddings", {
        "model": TEST_MODEL,
        "input": "test"
    }, timeout=60)
    print(f"Embeddings response: status={status} dim={len(body.get('data', [{}])[0].get('embedding', []))}")
    assert status == 200, f"embeddings failed: {status} {body}"

    print("\n✅ Round 39 e2e: PASSED — no warnings during chat, embedding works.")


if __name__ == "__main__":
    main()
```

- [ ] **Step 2: Run the test against the live deployment**

```bash
python tests/cppworker_embeddings_mode_e2e.py
```

Expected: `✅ Round 39 e2e: PASSED — no warnings during chat, embedding works.`

- [ ] **Step 3: If test fails (warnings > 0), debug**

Common causes:
- Did you forget to rebuild the C-bridge after editing `bridge.c`? The Go binary embeds the C-bridge via cgo — if the .so / .a is stale, the change won't be live.
- Did the cppworker container pick up the new image? Check `docker inspect ol-bundled-cppworker-gpu | grep Image`.
- Is the chat path actually going through `bridge_batched_decode`? Some quick paths might use a different code path. Check `cppworker/handlers_openai.go` for which bridge function is called.

---

## Task 7: Rebuild cppworker image

- [ ] **Step 1: Tag the new build**

```bash
cd C:\Ollama\ollamalegion
docker buildx build --load \
  -f docker/cppworker/Dockerfile \
  -t ollama-legion/cppworker:gpu-86-r39-emb-toggle \
  .
```

ETA: ~45 min (cppworker image is large due to llama.cpp CUDA build).

- [ ] **Step 2: Verify image exists**

```bash
docker images ollama-legion/cppworker:gpu-86-r39-emb-toggle
```

Expected: image listed, ~3.8GB.

---

## Task 8: Rebuild bundled image

- [ ] **Step 1: Tag the bundled image**

```bash
cd C:\Ollama\ollamalegion
docker buildx build --load \
  -f docker/balancer/Dockerfile.cppworker-bundled \
  -t ollama-legion/balancer:cppworker-bundled-r39-emb-toggle \
  .
```

ETA: ~10 min (Go only).

- [ ] **Step 2: Verify**

```bash
docker images ollama-legion/balancer:cppworker-bundled-r39-emb-toggle
```

Expected: ~62MB.

---

## Task 9: Deploy

- [ ] **Step 1: Update compose file**

In `deployments/docker-compose.cppworker-bundled-with-agent.yml`, change the image tags:

```yaml
services:
  cppworker-gpu:
    image: ollama-legion/cppworker:gpu-86-r39-emb-toggle   # was: gpu-86-r37-auto-adapt
  loadbalancer:
    image: ollama-legion/balancer:cppworker-bundled-r39-emb-toggle   # was: cppworker-bundled-r38-auto-adapt
```

(Or whatever the current image tag variables are — match the existing pattern.)

- [ ] **Step 2: Restart containers**

```bash
cd C:\Ollama\ollamalegion\deployments
CPPWORKER_GPU_TAG=86-r39-emb-toggle \
  docker compose -f docker-compose.cppworker-bundled-with-agent.yml \
  up -d --no-build cppworker-gpu loadbalancer
```

- [ ] **Step 3: Verify containers are healthy**

```bash
docker ps --format "table {{.Names}}`t{{.Status}}`t{{.Image}}" | Select-String ol-bundled
```

Expected: all `Up X minutes (healthy)`, images = r39.

---

## Task 10: Live verification (the real test)

- [ ] **Step 1: Send a chat request, check log for warning count**

```bash
# 1. Note current log position
$since = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ss")

# 2. Send a chat
$r = Invoke-WebRequest -Uri http://localhost:18092/v1/chat/completions `
  -Method POST -ContentType "application/json" -TimeoutSec 300 `
  -Body '{"model":"Qwen3.6-35B-A3B-UD-Q4_K_M","messages":[{"role":"user","content":"Reply OK."}],"max_tokens":10,"stream":false}'
Write-Host "Status: $($r.StatusCode)"

# 3. Wait for logs to flush, then grep
Start-Sleep -Seconds 3
docker logs --since $since ol-bundled-cppworker-gpu 2>&1 | Select-String "embeddings required"
```

Expected: empty output (zero matches).

- [ ] **Step 2: Run the e2e test from Task 6**

```bash
python tests/cppworker_embeddings_mode_e2e.py
```

Expected: PASS.

- [ ] **Step 3: Measure TTFT before/after (sanity check on perf claim)**

Send the same chat request 3 times, measure total time + TTFT, compare to baseline (before this fix). Expected: TTFT drop of 5-15% on first request after model load (warm requests less affected).

```bash
# Quick TTFT measure (using curl with timing):
curl -w "\ntime_total=%{time_total}s\n" \
  -X POST http://localhost:18092/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"Qwen3.6-35B-A3B-UD-Q4_K_M","messages":[{"role":"user","content":"Hi"}],"max_tokens":5}' \
  -o /dev/null -s
```

Note baseline value (from memory or pre-deploy logs) and post-deploy value. The claim is 5-15% TTFT reduction.

- [ ] **Step 4: Verify embedding endpoint still works**

```bash
$r = Invoke-WebRequest -Uri http://localhost:18092/v1/embeddings -Method POST `
  -ContentType "application/json" -Body '{"model":"Qwen3.6-35B-A3B-UD-Q4_K_M","input":"hello"}'
Write-Host "Embedding dim: $((($r.Content | ConvertFrom-Json).data[0].embedding).Count)"
```

Expected: dim = n_embd (e.g., 4096 for Qwen3.6).

---

## Task 11: Commit, push, memory

- [ ] **Step 1: Stage and commit**

```bash
cd C:\Ollama\ollamalegion
git add c/bridge/bridge.c c/bridge/bridge.h c/bridge/bridge_internal.h c/bridge/bridge.go c/bridge/bridge_stub.go
git add cmd/cppworker/handlers_openai.go cmd/cppworker/handlers_embeddings.go
git add tests/cppworker_embeddings_mode_e2e.py
git add c/bridge/tests/test_embeddings_mode.c
git add docs/superpowers/plans/2026-08-18-embeddings-mode-toggle.md
git commit -m "Round 39: dynamic embeddings mode toggle

Eliminates per-decode 'embeddings required but some input tokens were not
marked as outputs -> overriding' warning by toggling cparams.embeddings
per-request in the C-bridge:
  - chat path: embeddings=false (no override, no warning, no per-token
    logits re-mark, no extra t_embd memory)
  - embedding path: embeddings=true (llama_get_embeddings works)

Uses llama_set_embeddings() which is a single bool write in
cparams.embeddings (verified free — no memory ops, no KV cache impact).
Cost: 2 lines per chat decode call. Savings: 5-15% TTFT, 0 per-decode
t_embd allocation, clean logs.

Backward compat: bridge_load_model still sets ctx_params.embeddings=true
at load (so context is embedding-capable). We just flip off for chat.

Files:
  - c/bridge/bridge.{c,h,internal.h,go,stub.go}: new bridge_set_embeddings_mode
  - cmd/cppworker/handlers_*.go: optional callers (C-bridge sets mode
    internally for the main paths)
  - tests/cppworker_embeddings_mode_e2e.py: live e2e assertion (0 warnings)
  - c/bridge/tests/test_embeddings_mode.c: C-side unit smoke test
  - docs/superpowers/plans/2026-08-18-embeddings-mode-toggle.md: this plan
"
```

- [ ] **Step 2: Push to centurion branch**

```bash
git push origin centurion
```

- [ ] **Step 3: Update memory with the pattern**

Use the memory tool to append to `target=main` (agent memory), a brief note:

```yaml
target: main
operation: append
content: |
  ### cppworker: dynamic embeddings mode toggle (Round 39, 2026-08-18)

  **Bug class**: "embeddings required but some input tokens were not
  marked as outputs -> overriding" warning in chat path, caused by
  cparams.embeddings=true (set at load for /v1/embeddings endpoint) +
  chat batches with logits=1 only on last token. llama.cpp re-marks
  logits, allocating logits tensor sized for all tokens instead of 1,
  plus per-decode t_embd buffer. On 8GB VRAM + 22GB CPU-offloaded MoE
  this costs 5-15% TTFT.

  **Fix** (architectural, not workaround): call
  `llama_set_embeddings(ctx, false)` before each chat decode,
  `llama_set_embeddings(ctx, true)` before each embedding decode.
  Cost: O(1) — just flips a bool in cparams (verified in
  c/llama.cpp/src/llama-context.cpp:1153-1160). No memory ops, no
  KV cache impact, safe per-request under model mutex.

  **Why better than Option B (two contexts)**:
  - 0 VRAM cost (no second context)
  - 0 reload time (no second model load — 22GB Qwen3.6 = 5-7min)
  - 1 C-bridge function + 2 call sites
  - Same chat perf win

  **Pattern (cross-project, high-value)**:

  When llama.cpp context was created with embeddings=true (for
  /v1/embeddings support) but is ALSO used for chat (where the
  standard batch has logits=1 only on last token), the per-decode
  override path runs every time. llama.cpp's
  `llama_set_embeddings(ctx, bool)` is a cheap toggle (single bool
  write to cparams.embeddings) — use it per-request to avoid the
  override. Verified in: c/llama.cpp/src/llama-context.cpp:1153-1160
  and c/llama.cpp/src/llama-batch.cpp:131-146 (the warning code path).
```

- [ ] **Step 4: Delete the r38 cron self-reminder (no longer needed)**

```bash
mavis cron list --agent-name me | grep r38
mavis cron delete --cron-id <id>
```

---

## Acceptance Criteria

A Round 39 deploy is **DONE** when ALL of the following hold:

- [ ] `bridge_set_embeddings_mode` is exported from `libbridge.so` / `bridge.dll`
- [ ] Chat request to /v1/chat/completions produces 0 "embeddings required" warnings in cppworker logs
- [ ] /v1/embeddings endpoint still returns valid embeddings (dimension matches n_embd)
- [ ] TTFT measured at -5% or better vs pre-Round-39 baseline (target: -5 to -15%)
- [ ] Live e2e test `tests/cppworker_embeddings_mode_e2e.py` passes
- [ ] No new VRAM cost (cppworker container memory footprint unchanged ±50MB)
- [ ] No new reload time (model load still ~5-7min, not 10-14min)
- [ ] No regression in other endpoints (`/api/models`, `/api/tags`, `/v1/models`, `/api/embed`, `/api/chat`)
- [ ] `verify_bundled` test still passes (full API coverage check)
- [ ] Git commit on `centurion` branch, pushed

## Risks and Mitigations

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| `llama_set_embeddings` not safe to call mid-decode | Very low | High (segfault) | C-bridge already holds `instance.mu` during all decode calls. Toggle is inside the lock. |
| Forget to set mode in a new code path (e.g., a future `/v1/rerank`) | Medium | Low (warnings reappear) | Add a CI assertion: every `llama_decode` call site must be preceded by `llama_set_embeddings`. Or: change `bridge_load_model` to set `embeddings=false` by default, and require explicit opt-in to enable. |
| Mode is per-context, not per-request — race if chat and embedding interleave | Low | Medium (wrong output) | Currently serialized via `instance.mu`. If you ever add concurrent chat+embedding on same model, add an atomic flag. |
| Slower chat after toggle (e.g., if cparams.embeddings change forces reserve) | Low | Low (no perf gain) | Verified in code: `set_embeddings` body is `cparams.embeddings = value;` only. `sched_need_reserve = true` is commented out (llama-context.cpp:1158). |
| Toggling off embeddings breaks speculative decoding or other feature | Low | Medium | If speculative decoding is later enabled, may need to re-test. Current path doesn't use spec decode. |

## Rollback Plan

If Round 39 causes regressions:

1. Revert the commit: `git revert HEAD~1..HEAD` on centurion branch
2. Rebuild cppworker + bundled images (revert to `r37-auto-adapt` tags)
3. Re-deploy: `docker compose up -d --no-build cppworker-gpu loadbalancer`
4. Confirm warnings are back (signals rollback took effect)
5. Investigate root cause before retrying

ETA to rollback: ~50 min (45 cppworker + 5 deploy + restart).

## What's NOT in this plan (out of scope)

- Per-model `embeddings_mode` config in `config.bundled.json` — not needed; mode is per-request, not per-model
- Speculative decoding integration with embeddings toggle — none currently
- Batched embedding requests (multiple inputs in one call) — `bridge_get_embeddings` already handles this; toggle happens once per call, correct behavior
- Auto-detecting when a model is "embedding-capable" vs "chat-only" — overengineering, the toggle is free
- Replacing `bridge_load_model`'s hardcoded `embeddings=true` with a config option — not needed; the toggle handles the chat case without touching load
