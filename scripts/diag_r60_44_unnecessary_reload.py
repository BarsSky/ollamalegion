#!/usr/bin/env python3
"""R60.44 — reproduce: model loaded, OpenWebUI-style request triggers unnecessary reload."""
import requests
import time

NGINX = "http://localhost:18083"

# Two scenarios:
# A) model loaded with n_ctx=8192, request asks n_ctx=4096 (smaller) — should NOT reload
# B) model loaded with n_ctx=8192, request asks n_ctx=8192 (same) — should NOT reload
# C) model loaded with n_ctx=8192, request asks n_ctx=16384 (bigger) — should reload

def get_state():
    try:
        r = requests.get("http://localhost:18092/api/models", timeout=5)
        j = r.json()
        if j.get("count") == 1:
            return j["models"][0].get("state"), j["models"][0].get("context_size")
        return "unloaded", 0
    except Exception as e:
        return f"err: {e}", 0

def make_request(num_ctx, num_predict, label):
    body = {
        "model": "Qwen3-Instruct-2507-q4km",
        "messages": [{"role": "user", "content": "Привет"}],
        "stream": False,
        "options": {"num_ctx": num_ctx, "num_predict": num_predict, "temperature": 0},
    }
    state_before, ctx_before = get_state()
    start = time.time()
    try:
        r = requests.post(
            f"{NGINX}/api/chat", json=body, timeout=30,
            headers={"Authorization": "Bearer changeme-bundled-with-agent-token"},
        )
        elapsed = time.time() - start
        print(f"\n--- {label} ---")
        print(f"  BEFORE: state={state_before}, ctx={ctx_before}")
        print(f"  REQUEST: num_ctx={num_ctx}, num_predict={num_predict}")
        print(f"  RESPONSE: HTTP {r.status_code} in {elapsed:.1f}s, body_len={len(r.text)}")
        print(f"  Retry-After: {r.headers.get('Retry-After')}")
        if r.status_code == 200:
            d = r.json()
            msg = d.get("message", {})
            content = msg.get("content", "") if isinstance(msg, dict) else ""
            print(f"  Content: {content[:80]!r}")
        else:
            print(f"  Body: {r.text[:200]}")
        return r.status_code
    except requests.exceptions.Timeout:
        print(f"\n--- {label} ---")
        print(f"  TIMEOUT after 30s")
        return 0

# Wait for cppworker to be ready
print("Initial state:", get_state())

# Scenario A: smaller n_ctx
make_request(4096, 32, "Scenario A: n_ctx=4096 < loaded 8192")
time.sleep(2)
print(f"\nState after A: {get_state()}")

# Scenario B: same n_ctx
make_request(8192, 32, "Scenario B: n_ctx=8192 = loaded 8192")
time.sleep(2)
print(f"\nState after B: {get_state()}")

# Scenario C: bigger n_ctx
make_request(16384, 32, "Scenario C: n_ctx=16384 > loaded 8192 (should reload)")
time.sleep(2)
print(f"\nState after C: {get_state()}")
