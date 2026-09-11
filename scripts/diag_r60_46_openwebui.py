#!/usr/bin/env python3
"""R60.46 — test with EXACT OpenWebUI default settings."""
import requests
import time

PROMPT = "Привет распиши красивый сайт на html css для интерактивной математики расчета движения полета"

# OpenWebUI default settings:
# - num_ctx: 2048 (or 8192 if user set higher)
# - num_predict: -1 (unlimited) or 2048
# - stream: True

# Test A: Ollama format, num_ctx=2048, num_predict=2048, STREAM
print("=" * 70)
print("Test A: Ollama /api/chat, n_ctx=2048, n_predict=2048, STREAM")
print("=" * 70)
body = {
    "model": "Qwen3-Instruct-2507-q4km",
    "messages": [{"role": "user", "content": PROMPT}],
    "stream": True,
    "options": {"num_ctx": 2048, "num_predict": 2048, "temperature": 0.7},
}
start = time.time()
try:
    r = requests.post(
        "http://localhost:18083/api/chat",
        json=body,
        timeout=60,
        stream=True,
        headers={"Authorization": "Bearer changeme-bundled-with-agent-token"},
    )
    print(f"  HTTP {r.status_code} ({r.elapsed.total_seconds():.1f}s)")
    print(f"  Content-Type: {r.headers.get('Content-Type')}")

    if r.status_code != 200:
        print(f"  Body: {r.text[:300]}")
    else:
        chunks = []
        full = ""
        for line in r.iter_lines():
            if line:
                text = line.decode("utf-8", errors="ignore")
                chunks.append(text)
                if '"content":"' in text:
                    import re
                    m = re.search(r'"content":"([^"]*)"', text)
                    if m:
                        full += m.group(1)
        elapsed = time.time() - start
        print(f"  chunks: {len(chunks)}, full_content_len: {len(full)}, elapsed: {elapsed:.1f}s")
        if chunks:
            print(f"  first: {chunks[0]}")
            print(f"  last: {chunks[-1][:300]}")
        if full:
            print(f"  content[:200]: {full[:200]!r}")
except Exception as e:
    print(f"  ERROR: {e}")

# Test B: OpenAI format, max_tokens=-1 or large, STREAM
print()
print("=" * 70)
print("Test B: OpenAI /v1/chat/completions, max_tokens=2048, STREAM")
print("=" * 70)
body2 = {
    "model": "Qwen3-Instruct-2507-q4km",
    "messages": [{"role": "user", "content": PROMPT}],
    "stream": True,
    "max_tokens": 2048,
    "temperature": 0.7,
}
start = time.time()
try:
    r = requests.post(
        "http://localhost:18083/v1/chat/completions",
        json=body2,
        timeout=60,
        stream=True,
        headers={"Authorization": "Bearer changeme-bundled-with-agent-token"},
    )
    print(f"  HTTP {r.status_code} ({r.elapsed.total_seconds():.1f}s)")

    if r.status_code != 200:
        print(f"  Body: {r.text[:300]}")
    else:
        chunks = []
        full = ""
        for line in r.iter_lines():
            if line:
                text = line.decode("utf-8", errors="ignore")
                chunks.append(text)
                if '"content":"' in text:
                    import re
                    m = re.search(r'"content":"([^"]*)"', text)
                    if m:
                        full += m.group(1).replace("\\n", "\n")
        elapsed = time.time() - start
        print(f"  chunks: {len(chunks)}, full_content_len: {len(full)}, elapsed: {elapsed:.1f}s")
        if full:
            print(f"  content[:200]: {full[:200]!r}")
except Exception as e:
    print(f"  ERROR: {e}")
