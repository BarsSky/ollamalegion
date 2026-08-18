# Round 37 e2e tests — Cline-like auto-adapt n_ctx

## Files
- `test_round37_auto_n_ctx.py` — main e2e: Cline-like request (num_ctx=65536)
  against balancer, verifies no 413, checks /api/models exposes feasible_max_context.

## Production bug (2026-08-18)
```
Cline: POST /v1/chat/completions {model, num_ctx: 65536, ...}
Balancer: 413 "preflight: prompt + n_predict exceeds n_ctx for this backend"
  reason: "requested n_ctx=65536 exceeds model max context=32768"
  (profile Qwen3.6-35B contextLength=32768, GGUF supports 262144)
```

## Round 37 fix
1. **cppworker** `/api/models` exposes `feasible_max_context` and `gguf_max_context`
2. **cppworker** CLI `-feasible <model>` — operator inspection
3. **cppworker** CLI `-auto-load <model>` — auto-detect + load
4. **cppworker** background `feasibleSync` — warns when profile conservative
5. **balancer** `resolveModelMaxContext` v2 — 3-tier (profile / feasible / GGUF)
6. **balancer** `preflight_helper` — auto-relaxes when `profile.contextLengthAuto=true`
7. **profile schema** — `contextLengthAuto: true` + `contextLengthMax: N` fields

## Usage
```bash
# Default: balancer on :18080, cppworker on :18092, Qwen3.6 model
python test_round37_auto_n_ctx.py

# Custom endpoints
python test_round37_auto_n_ctx.py --balancer http://1.2.3.4:18080 \
                                    --cppworker http://1.2.3.4:18092 \
                                    --model my-model
```

## Expected output
```
[STEP 1] GET http://localhost:18092/api/models
  feasible_max_context = 65536
  gguf_max_context     = 262144
  ✅ cppworker reports feasible_max_context = 65536

[STEP 2] POST http://localhost:18080/v1/chat/completions (Cline-like)
  status = 200
  ✅ PASS: 200 OK, content preview: 'OK'

[STEP 3] POST http://localhost:18080/v1/chat/completions (STREAMING)
  status = 200, chunks = 5
  ✅ PASS: streaming works with num_ctx=65536

✅ ALL CHECKS PASSED — Round 37 fix is working
```

## Pre-Round 37 output (FAIL)
```
[STEP 2] POST http://localhost:18080/v1/chat/completions (Cline-like)
  status = 413
  ❌ FAIL: 413 CONSERVATIVE PROFILE
     body: {"error":"preflight: ... exceeds model max context=32768"...}
```

## CI integration
This test should be run:
- After every cppworker/balancer build
- Before release
- As smoke test in deployment verification

It catches the EXACT class of bug from 2026-08-18 (profile conservative + 413).
