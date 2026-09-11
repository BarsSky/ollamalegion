#!/usr/bin/env python3
"""R60.46b — verify all scenarios work."""
import requests
import time

def test(label, body, timeout=60):
    start = time.time()
    try:
        r = requests.post(
            "http://localhost:18083/api/chat",
            json=body,
            timeout=timeout,
            headers={"Authorization": "Bearer changeme-bundled-with-agent-token"},
        )
        elapsed = time.time() - start
        if r.status_code == 200:
            d = r.json()
            content = d.get("message", {}).get("content", "")
            print(f"  ✅ {label}: HTTP 200 in {elapsed:.1f}s, content_len={len(content)}, eval_count={d.get('eval_count')}")
        else:
            print(f"  ❌ {label}: HTTP {r.status_code} in {elapsed:.1f}s, body={r.text[:200]}")
    except Exception as e:
        print(f"  ❌ {label}: EXCEPTION {e}")

print("Scenario 1: num_ctx=2048, no n_predict (OpenWebUI default)")
test("default", {
    "model": "Qwen3-Instruct-2507-q4km",
    "messages": [{"role": "user", "content": "Привет"}],
    "stream": False,
    "options": {"num_ctx": 2048, "temperature": 0},
})

print()
print("Scenario 2: num_ctx=2048, n_predict=64 (explicit small)")
test("explicit small n_predict", {
    "model": "Qwen3-Instruct-2507-q4km",
    "messages": [{"role": "user", "content": "Привет"}],
    "stream": False,
    "options": {"num_ctx": 2048, "num_predict": 64, "temperature": 0},
})

print()
print("Scenario 3: num_ctx=2048, n_predict=2048 (matches loaded, edge case)")
test("n_predict=2048", {
    "model": "Qwen3-Instruct-2507-q4km",
    "messages": [{"role": "user", "content": "Привет"}],
    "stream": False,
    "options": {"num_ctx": 2048, "num_predict": 2048, "temperature": 0},
})

print()
print("Scenario 4: num_ctx=2048, n_predict=4096 (overflow!)")
test("n_predict=4096 (should trigger reload)", {
    "model": "Qwen3-Instruct-2507-q4km",
    "messages": [{"role": "user", "content": "Привет"}],
    "stream": False,
    "options": {"num_ctx": 2048, "num_predict": 4096, "temperature": 0},
})

print()
print("Scenario 5: num_ctx=8192, no n_predict (upgrade)")
test("upgrade to 8192", {
    "model": "Qwen3-Instruct-2507-q4km",
    "messages": [{"role": "user", "content": "Привет"}],
    "stream": False,
    "options": {"num_ctx": 8192, "temperature": 0},
})
