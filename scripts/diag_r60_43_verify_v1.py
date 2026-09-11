#!/usr/bin/env python3
"""R60.43 — verify /v1/ endpoints work via nginx (OpenWebUI path)."""
import requests
import time

NGINX = "http://localhost:18083"
body = {
    "model": "Qwen3-Instruct-2507-q4km",
    "messages": [{"role": "user", "content": "Привет, как дела?"}],
    "stream": False,
}

# Test 1: /v1/chat/completions (was 405 before)
print("=== Test 1: /v1/chat/completions (was 405 HTML before fix) ===")
r = requests.post(
    f"{NGINX}/v1/chat/completions",
    json=body,
    timeout=60,
    headers={"Authorization": "Bearer changeme-bundled-with-agent-token"},
)
print(f"HTTP {r.status_code} ({r.elapsed.total_seconds():.1f}s)")
print(f"Content-Type: {r.headers.get('Content-Type')}")
if r.status_code == 200:
    d = r.json()
    print(f"choices[0]: {d['choices'][0]['message']['content'][:200]!r}")
    print(f"usage: {d.get('usage')}")
else:
    print(f"Body: {r.text[:300]}")

# Test 2: /openai/v1/chat/completions (existing route, should still work)
print("\n=== Test 2: /openai/v1/chat/completions (existing route, should work) ===")
r = requests.post(
    f"{NGINX}/openai/v1/chat/completions",
    json=body,
    timeout=60,
    headers={"Authorization": "Bearer changeme-bundled-with-agent-token"},
)
print(f"HTTP {r.status_code} ({r.elapsed.total_seconds():.1f}s)")
if r.status_code == 200:
    d = r.json()
    print(f"choices[0]: {d['choices'][0]['message']['content'][:200]!r}")

# Test 3: /v1/models (OpenWebUI uses this to list models)
print("\n=== Test 3: /v1/models ===")
r = requests.get(
    f"{NGINX}/v1/models",
    timeout=10,
    headers={"Authorization": "Bearer changeme-bundled-with-agent-token"},
)
print(f"HTTP {r.status_code} ({r.elapsed.total_seconds():.1f}s)")
print(f"Body[:200]: {r.text[:200]}")
