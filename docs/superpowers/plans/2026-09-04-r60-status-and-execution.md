# R60 Embeddings Mode Toggle — Status & Remaining Execution Plan

> **Date:** 2026-09-04
> **Status:** Code 100% DONE, deployment pending
> **Reference plan:** [2026-08-18-embeddings-mode-toggle.md](2026-08-18-embeddings-mode-toggle.md) (770 lines, 11 tasks)
> **Goal of this doc:** status snapshot + execution steps for the remaining 4 deployment tasks (7-10) + push (11.2).

## Status Snapshot (verified 2026-09-04)

| Task | What | Status | Commit |
|------|------|--------|--------|
| 1 | C-bridge `embeddings_mode` field + setter declaration | ✅ DONE | `3a2901c` (R39) |
| 2 | C-bridge chat path — `llama_set_embeddings(false)` before decode | ✅ DONE | `3a2901c` (R39) |
| 3 | C-bridge embedding path — `llama_set_embeddings(true)` at entry | ✅ DONE | `3a2901c` (R39) |
| 4 | Go `BridgeMode` + `SetEmbeddingsMode` wrapper + stub | ✅ DONE | `3a2901c` (R39) |
| 5 | Standalone C unit test | ✅ DONE | `b5b8c88` (R60.1) |
| 6 | Live e2e Python test (log scrape) | ✅ DONE | `3a2901c` (R39) |
| 7 | Rebuild cppworker image (docker) | ⏳ TODO | — |
| 8 | Rebuild bundled image (docker) | ⏳ TODO | — |
| 9 | Deploy to running infra | ⏳ TODO | — |
| 10 | Live verification (chat warning count, TTFT, embedding still works) | ⏳ TODO | — |
| 11.1 | Commit + push | ½ DONE (commit done, push pending) | `3a2901c`, `b5b8c88` |
| 11.3 | Memory entry | ✅ DONE | (this session, `mavis memory` tool) |
| 11.4 | Delete r38 cron | N/A | r38 cron never existed |

**Code-level completion: 100%.** All 6 code/test tasks (1-6) committed on `centurion`. Memory saved with the cross-project pattern.

**Remaining work: 4 deployment tasks (7-10) + push (11.2).** All require running infrastructure.

## Why this matters (perf claim recap)

For Qwen3.6 22GB on 8GB VRAM (CPU offloaded):
- **Before**: per-decode `embeddings required → overriding` warning fires 200+ times per chat request, forces 200×n_vocab logits tensor allocation + t_embd buffer alloc per decode. TTFT tax: 5-15%.
- **After**: chat path sets `cparams.embeddings = false` (1 bool write, O(1), no memory ops), warning never fires, logis sized for 1 token.
- **Cost**: 2 lines per chat decode call.
- **Source**: `c/llama.cpp/src/llama-context.cpp:1153-1160` (the `llama_set_embeddings` body is literally `cparams.embeddings = value;`).

## Remaining Execution Plan

### Task 7: Rebuild cppworker image ⏳

**Prerequisites:**
- Docker installed (verified: `docker --version`)
- GPU available for CUDA build (RTX 3070/4080/4090/A10/RTX 5090 — sm_86/89/120)
- ~45 min ETA (cppworker image is ~3.8GB due to llama.cpp CUDA build)

**Step 7.1 — Tag the new build:**
```powershell
cd C:\Ollama\ollamalegion
docker buildx build --load `
  -f docker/cppworker/Dockerfile `
  -t ollama-legion/cppworker:gpu-86-r60-emb-toggle `
  .
```

**Step 7.2 — Verify:**
```powershell
docker images ollama-legion/cppworker:gpu-86-r60-emb-toggle
# Expected: image listed, ~3.8GB
```

**Risk:** cuda_arch mismatch if hardware changed. Check `CUDA_ARCH` in `deployments/.env.bundled-with-agent` (likely 86 for RTX 3070/A10).

---

### Task 8: Rebuild bundled image ⏳

**Prerequisites:**
- Task 7 complete (bundled image is built FROM cppworker image)
- ~10 min ETA (Go only)

**Step 8.1 — Tag the bundled image:**
```powershell
cd C:\Ollama\ollamalegion
docker buildx build --load `
  -f docker/balancer/Dockerfile.cppworker-bundled `
  -t ollama-legion/balancer:cppworker-bundled-r60-emb-toggle `
  .
```

**Step 8.2 — Verify:**
```powershell
docker images ollama-legion/balancer:cppworker-bundled-r60-emb-toggle
# Expected: ~62MB
```

**Reference:** `docker/cppworker/Dockerfile.gpu-r51.6` (committed in `8950a6d`) shows the R51.6 incremental rebuild pattern. For R60, the C-bridge change is in a recompiled C-bridge, so full cppworker rebuild is needed (not incremental).

---

### Task 9: Deploy ⏳

**Prerequisites:**
- Tasks 7 + 8 complete
- Current deployment uses pre-R60 images (likely `gpu-86-r37-auto-adapt` or similar)

**Step 9.1 — Update compose file:**

In `deployments/docker-compose.cppworker-bundled-with-agent.yml`, change image tags:
```yaml
services:
  cppworker-gpu:
    image: ollama-legion/cppworker:gpu-86-r60-emb-toggle   # was: gpu-86-rXX-pre-r60
  loadbalancer:
    image: ollama-legion/balancer:cppworker-bundled-r60-emb-toggle   # was: rXX-pre-r60
```

Match the existing pattern (use grep to find current image tag variables).

**Step 9.2 — Restart containers:**
```powershell
cd C:\Ollama\ollamalegion\deployments
$env:CPPWORKER_GPU_TAG = "86-r60-emb-toggle"
docker compose -f docker-compose.cppworker-bundled-with-agent.yml `
  up -d --no-build cppworker-gpu loadbalancer
```

**Step 9.3 — Verify health:**
```powershell
docker ps --format "table {{.Names}}\t{{.Status}}\t{{.Image}}" | Select-String ol-bundled
# Expected: all "Up X minutes (healthy)", images = r60-emb-toggle
```

**ETA:** ~5 min (container restart + healthcheck).

---

### Task 10: Live verification ⏳

**Prerequisites:**
- Task 9 complete
- A model loaded (Qwen3.6 22GB Q4_K_M is the canonical test target)
- 5-15 min for model load (one-time per restart)

**Step 10.1 — Send chat, check warning count:**
```powershell
# 1. Note log position
$since = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ss")

# 2. Send chat
$r = Invoke-WebRequest -Uri http://localhost:18092/v1/chat/completions `
  -Method POST -ContentType "application/json" -TimeoutSec 300 `
  -Body '{"model":"Qwen3.6-35B-A3B-UD-Q4_K_M","messages":[{"role":"user","content":"Reply OK."}],"max_tokens":10,"stream":false}'
Write-Host "Status: $($r.StatusCode)"

# 3. Wait for log flush, then grep
Start-Sleep -Seconds 3
docker logs --since $since ol-bundled-cppworker-gpu 2>&1 | Select-String "embeddings required"
# Expected: empty output (zero matches)
```

**Step 10.2 — Run e2e test:**
```powershell
python tests/cppworker_embeddings_mode_e2e.py
# Expected: PASS
```

**Step 10.3 — TTFT measurement (perf sanity check):**
```powershell
# Send 3 requests, measure time_total
1..3 | ForEach-Object {
  curl -w "Request $_: time_total=%{time_total}s`n" `
    -X POST http://localhost:18092/v1/chat/completions `
    -H "Content-Type: application/json" `
    -d '{"model":"Qwen3.6-35B-A3B-UD-Q4_K_M","messages":[{"role":"user","content":"Hi"}],"max_tokens":5}' `
    -o $null -s
}
# Compare to pre-R60 baseline (look in balancer.log or memory)
# Target: -5 to -15% on first request after load
```

**Step 10.4 — Verify embedding endpoint still works:**
```powershell
$r = Invoke-WebRequest -Uri http://localhost:18092/v1/embeddings -Method POST `
  -ContentType "application/json" `
  -Body '{"model":"Qwen3.6-35B-A3B-UD-Q4_K_M","input":"hello"}'
$emb = ($r.Content | ConvertFrom-Json).data[0].embedding
Write-Host "Embedding dim: $($emb.Count)"
# Expected: dim = n_embd (4096 for Qwen3.6)
```

**Step 10.5 — No regression check on other endpoints:**
```powershell
# /api/tags, /api/ps, /v1/models, /api/embed, /api/chat, /api/models
# Quick smoke: all should return 200 OK
$endpoints = @("/api/tags", "/api/ps", "/v1/models", "/api/models", "/api/embed")
foreach ($ep in $endpoints) {
  $r = Invoke-WebRequest -Uri "http://localhost:18092$ep" -TimeoutSec 30
  Write-Host "$ep -> $($r.StatusCode)"
}
```

---

### Task 11.2: Push to centurion ⏳

**Prerequisites:**
- All other tasks complete (or at least Task 10.1 passed)

**Step 11.2.1 — Push commits:**
```powershell
cd C:\Ollama\ollamalegion
git push origin centurion
# Expected: 7 commits pushed (R59.14..R60.1 + the artifact commits)
```

**Step 11.2.2 — Verify GitHub branch is current:**
- Check github.com/BarsSky/ollamalegion/tree/centurion has the new commits
- Check CI is green on the branch

---

## Acceptance Criteria (from original plan, restated for clarity)

A Round 60 deploy is **DONE** when ALL of the following hold:

- [ ] `bridge_set_embeddings_mode` is exported from `bridge.dll` (verify with `dumpbin /EXPORTS`)
- [ ] Chat request to /v1/chat/completions produces 0 "embeddings required" warnings in cppworker logs
- [ ] /v1/embeddings endpoint still returns valid embeddings (dimension matches n_embd)
- [ ] TTFT measured at -5% or better vs pre-R60 baseline (target: -5 to -15%)
- [ ] `tests/cppworker_embeddings_mode_e2e.py` passes
- [ ] No new VRAM cost (cppworker container memory footprint unchanged ±50MB)
- [ ] No new reload time (model load still ~5-7min, not 10-14min)
- [ ] No regression in other endpoints
- [ ] `git push origin centurion` succeeds
- [ ] CI green on `centurion` branch

## Risks and Mitigations (carried over from original plan)

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| `llama_set_embeddings` not safe mid-decode | Very low | High (segfault) | C-bridge holds `instance.mu` during decode. Toggle is inside the lock. |
| Forget to set mode in a new code path | Medium | Low (warnings reappear) | Add a CI grep: every `llama_decode` call site must be preceded by `llama_set_embeddings`. Or: change `bridge_load_model` to set `embeddings=false` by default. |
| Race if chat and embedding interleave on same model | Low | Medium | Serialized via `instance.mu`. If concurrent, add atomic flag. |
| Slower chat after toggle | Low | Low | Verified: `set_embeddings` body is `cparams.embeddings = value;` only. `sched_need_reserve = true` is commented out. |

## Rollback Plan

If R60 causes regressions:

1. `git revert HEAD~N..HEAD` on centurion (revert R59.14..R60.1 commits)
2. Rebuild cppworker + bundled images (revert to pre-R60 tags)
3. Re-deploy: `docker compose up -d --no-build cppworker-gpu loadbalancer`
4. Confirm warnings are back (signals rollback took effect)
5. Investigate root cause before retrying

ETA to rollback: ~50 min (45 cppworker + 5 deploy + restart).

## Out of Scope (carried over from original plan)

- Per-model `embeddings_mode` config in `config.bundled.json` — not needed
- Speculative decoding integration with embeddings toggle — not currently used
- Batched embedding requests — already handled
- Auto-detecting "embedding-capable" vs "chat-only" — overengineering

## Time Estimate

- Task 7: 45 min (cppworker image rebuild)
- Task 8: 10 min (bundled image rebuild)
- Task 9: 5 min (deploy + healthcheck)
- Task 10: 10-15 min (model load + verifications)
- Task 11.2: 2 min (git push + CI check)

**Total ETA: ~70 min** (single uninterrupted session with running infra).

## Prerequisites to Start

- [ ] Docker daemon running
- [ ] RTX 30xx/40xx/50xx or A10 GPU available
- [ ] `deployments/.env.bundled-with-agent` has correct `CUDA_ARCH` for hardware
- [ ] Current deployment healthy (rollback target exists)
- [ ] Time slot of 70+ min uninterrupted

If any prerequisite is missing, defer R60 to a later session when infra is ready.
