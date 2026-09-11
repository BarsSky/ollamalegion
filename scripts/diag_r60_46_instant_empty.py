#!/usr/bin/env python3
"""R60.46 — capture EXACTLY what user sees. Multiple paths, full instrumentation."""
import requests
import time
import json

PROMPT = "Привет распиши красивый сайт на html css для интерактивной математики расчета движения полета"

# Test 1: Direct to balancer /api/chat (Ollama format)
print("=" * 70)
print("Test 1: POST /api/chat (Ollama format) via nginx")
print("=" * 70)
body = {
    "model": "Qwen3-Instruct-2507-q4km",
    "messages": [{"role": "user", "content": PROMPT}],
    "stream": False,
    "options": {"num_ctx": 4096, "num_predict": 32, "temperature": 0},
}
start = time.time()
r = requests.post(
    "http://localhost:18083/api/chat",
    json=body,
    timeout=30,
    headers={"Authorization": "Bearer changeme-bundled-with-agent-token"},
)
elapsed = time.time() - start
print(f"  HTTP {r.status_code} in {elapsed:.1f}s")
print(f"  Content-Type: {r.headers.get('Content-Type')}")
print(f"  Retry-After: {r.headers.get('Retry-After')}")
print(f"  X-Backend-Id: {r.headers.get('X-Backend-Id')}")
print(f"  Body len: {len(r.text)}")
print(f"  Body[:500]: {r.text[:500]}")

# Test 2: Direct to balancer /v1/chat/completions (OpenAI format)
print()
print("=" * 70)
print("Test 2: POST /v1/chat/completions (OpenAI format) via nginx")
print("=" * 70)
body2 = {
    "model": "Qwen3-Instruct-2507-q4km",
    "messages": [{"role": "user", "content": PROMPT}],
    "stream": False,
    "max_tokens": 32,
    "temperature": 0,
}
start = time.time()
r = requests.post(
    "http://localhost:18083/v1/chat/completions",
    json=body2,
    timeout=30,
    headers={"Authorization": "Bearer changeme-bundled-with-agent-token"},
)
elapsed = time.time() - start
print(f"  HTTP {r.status_code} in {elapsed:.1f}s")
print(f"  Content-Type: {r.headers.get('Content-Type')}")
print(f"  Body len: {len(r.text)}")
print(f"  Body[:500]: {r.text[:500]}")

# Test 3: Direct to balancer (bypass nginx)
print()
print("=" * 70)
print("Test 3: POST /api/chat DIRECTLY to balancer (no nginx)")
print("=" * 70)
start = time.time()
r = requests.post(
    "http://localhost:18080/api/chat",
    json=body,
    timeout=30,
    headers={"Authorization": "Bearer bundled-default"},
)
elapsed = time.time() - start
print(f"  HTTP {r.status_code} in {elapsed:.1f}s")
print(f"  Content-Type: {r.headers.get('Content-Type')}")
print(f"  Body len: {len(r.text)}")
print(f"  Body[:500]: {r.text[:500]}")

# Test 4: Direct to cppworker
print()
print("=" * 70)
print("Test 4: POST /v1/chat/completions DIRECTLY to cppworker")
print("=" * 70)
start = time.time()
r = requests.post(
    "http://localhost:18092/v1/chat/completions",
    json=body2,
    timeout=30,
    headers={"Authorization": "Bearer changeme-bundled-with-agent-token"},
)
elapsed = time.time() - start
print(f"  HTTP {r.status_code} in {elapsed:.1f}s")
print(f"  Content-Type: {r.headers.get('Content-Type')}")
print(f"  Body len: {len(r.text)}")
print(f"  Body[:500]: {r.text[:500]}")
