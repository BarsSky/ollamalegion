#!/usr/bin/env python3
import requests
body = {
    "model": "Qwen3-Instruct-2507-q4km",
    "messages": [{"role": "user", "content": "Привет"}],
    "stream": False,
    "options": {"num_ctx": 16384, "num_predict": 32, "temperature": 0},
}
r = requests.post(
    "http://localhost:18083/api/chat",
    json=body,
    timeout=30,
    headers={"Authorization": "Bearer changeme-bundled-with-agent-token"},
)
print(f"After reload, request with num_ctx=16384: HTTP {r.status_code} ({r.elapsed.total_seconds():.1f}s)")
if r.status_code == 200:
    d = r.json()
    content = d["message"]["content"]
    print(f"content: {content!r}")
else:
    print(f"Body: {r.text[:200]}")
