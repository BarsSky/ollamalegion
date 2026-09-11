#!/usr/bin/env python3
import requests
import time
body = {
    "model": "Qwen3-Instruct-2507-q4km",
    "messages": [{"role": "user", "content": "Привет"}],
    "stream": False,
    "options": {"num_ctx": 2048, "temperature": 0.7},
}
start = time.time()
r = requests.post(
    "http://localhost:18083/api/chat",
    json=body,
    timeout=60,
    headers={"Authorization": "Bearer changeme-bundled-with-agent-token"},
)
elapsed = time.time() - start
print(f"num_ctx=2048, no n_predict: HTTP {r.status_code} in {elapsed:.1f}s, len={len(r.text)}")
if r.status_code == 200:
    d = r.json()
    content = d.get("message", {}).get("content", "")
    eval_count = d.get("eval_count", 0)
    print(f"  content[:80]: {content[:80]!r}")
    print(f"  eval_count: {eval_count}")
else:
    print(f"  body: {r.text[:300]}")
