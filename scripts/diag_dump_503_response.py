#!/usr/bin/env python3
"""Dump raw 503 response to understand structure."""
import requests
import json

body = {
    "model": "Qwen3-Instruct-2507-q4km",
    "messages": [{"role": "user", "content": "Привет"}],
    "stream": False,
    "options": {"num_ctx": 4096, "num_predict": 32, "temperature": 0},
}

print("Sending request...")
r = requests.post("http://localhost:18080/api/chat", json=body, timeout=30)
print(f"HTTP {r.status_code}")
print(f"Headers: {dict(r.headers)}")
print(f"\nBody (raw):")
print(r.text)
print(f"\nBody (parsed):")
try:
    print(json.dumps(r.json(), indent=2, ensure_ascii=False))
except Exception as e:
    print(f"NOT JSON: {e}")
