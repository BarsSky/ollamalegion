#!/usr/bin/env python3
import requests
import json

# n_ctx=200000 exceeds feasible_max_context=50255 - should trigger reload rejection
body = {
    "model": "Qwen3-Instruct-2507-q4km",
    "messages": [{"role": "user", "content": "Hi"}],
    "stream": False,
    "options": {"num_ctx": 200000, "num_predict": 512, "temperature": 0},
}
r = requests.post("http://localhost:18080/api/chat", json=body, timeout=15)
print(f"HTTP {r.status_code}")
print(f"Retry-After: {r.headers.get('Retry-After')}")
print(f"X-Backend-Id: {r.headers.get('X-Backend-Id')}")
print("Body:")
print(json.dumps(r.json(), indent=2, ensure_ascii=False))
