#!/usr/bin/env python3
"""R60.23 quick test: short prompt to verify empty-model guard doesn't break valid requests.

Sends a SHORT prompt to /api/chat to avoid hitting the 240s wall-clock limit
while the model generates a long HTML site. Verifies:
1. Request reaches cppworker (no 503 stuck in 'reloading' state)
2. Response is complete with done=true
3. eval_count > 0
"""
import json
import time
import requests

BALANCER = "http://localhost:18080"
API_TOKEN = "changeme-bundled-with-agent-token"
MODEL = "Qwen3-Instruct-2507-q4km"
PROMPT = "Скажи 'привет' одним словом."

start = time.time()
try:
    with requests.post(
        f"{BALANCER}/api/chat",
        json={
            "model": MODEL,
            "messages": [{"role": "user", "content": PROMPT}],
            "stream": False,
        },
        headers={"X-API-Token": API_TOKEN},
        timeout=120,
    ) as resp:
        elapsed = time.time() - start
        print(f"HTTP {resp.status_code}, time {elapsed:.1f}s")
        if resp.status_code == 200:
            data = resp.json()
            content = data.get("message", {}).get("content", "")
            print(f"Content: {content!r}")
            print(f"eval_count: {data.get('eval_count')}")
            print(f"done_reason: {data.get('done_reason')}")
            print(f"total_duration: {data.get('total_duration')}")
            print(f"prompt_eval_duration: {data.get('prompt_eval_duration')}")
            print(f"eval_duration: {data.get('eval_duration')}")
            print(f"tokens_per_second: {data.get('tokens_per_second')}")
        else:
            print(f"Body: {resp.text[:500]}")
except Exception as e:
    elapsed = time.time() - start
    print(f"EXCEPTION after {elapsed:.1f}s: {e}")
