#!/usr/bin/env python3
import requests
body = {
    "model": "Qwen3-Instruct-2507-q4km",
    "messages": [{"role": "user", "content": "Привет"}],
    "stream": False,
}
r = requests.post(
    "http://localhost:18083/v1/chat/completions",
    json=body,
    timeout=10,
    headers={"Authorization": "Bearer changeme-bundled-with-agent-token"},
)
print(f"HTTP {r.status_code} ({r.elapsed.total_seconds():.1f}s)")
print(f"Content-Type: {r.headers.get('Content-Type')}")
print(f"Body: {r.text[:500]}")
