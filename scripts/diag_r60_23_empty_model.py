#!/usr/bin/env python3
"""R60.23 verify empty-model guard.

Sends POST /api/load (or any non-model endpoint) and verifies that:
1. The balancer does NOT 503 with "model '' is being reloaded"
2. The balancer does NOT trigger an empty-name reload to cppworker
3. Either proxies to cppworker (returning 404 since /api/load doesn't exist)
   OR returns a clean 4xx without cascading failures.
"""
import time
import requests

BALANCER = "http://localhost:18080"
API_TOKEN = "changeme-bundled-with-agent-token"

# Test 1: empty body
print("=== Test 1: POST /api/load with empty body ===")
start = time.time()
try:
    resp = requests.post(
        f"{BALANCER}/api/load",
        data="",
        headers={"X-API-Token": API_TOKEN, "Content-Type": "application/json"},
        timeout=30,
    )
    print(f"HTTP {resp.status_code}, time {time.time()-start:.1f}s")
    print(f"Body: {resp.text[:200]}")
except Exception as e:
    print(f"EXCEPTION: {e}")

# Test 2: body with no model field
print("\n=== Test 2: POST /api/load with no model field ===")
start = time.time()
try:
    resp = requests.post(
        f"{BALANCER}/api/load",
        json={"something": "else"},
        headers={"X-API-Token": API_TOKEN},
        timeout=30,
    )
    print(f"HTTP {resp.status_code}, time {time.time()-start:.1f}s")
    print(f"Body: {resp.text[:200]}")
except Exception as e:
    print(f"EXCEPTION: {e}")

# Test 3: real chat request (must still work after empty-model guard)
print("\n=== Test 3: valid /api/chat request ===")
start = time.time()
try:
    resp = requests.post(
        f"{BALANCER}/api/chat",
        json={
            "model": "Qwen3-Instruct-2507-q4km",
            "messages": [{"role": "user", "content": "ОК"}],
            "stream": False,
        },
        headers={"X-API-Token": API_TOKEN},
        timeout=30,
    )
    print(f"HTTP {resp.status_code}, time {time.time()-start:.1f}s")
    if resp.status_code == 200:
        data = resp.json()
        print(f"Content: {data.get('message', {}).get('content', '')!r}")
except Exception as e:
    print(f"EXCEPTION: {e}")
