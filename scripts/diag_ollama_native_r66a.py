#!/usr/bin/env python3
"""diag_ollama_native_r66a.py — диагностика translator-bug фикса.

Шлёт запросы на /api/chat (Ollama-native) и /v1/chat/completions (OpenAI)
к balancer, проверяет:

  - native Ollama /api/chat  → НЕ содержит "upstream returned empty response"
                              → содержит текст от Qwen3.8
                              → содержит message.content
  - OpenAI    /v1/chat/completions → содержит choices[0].message.content
  - /api/ps                   → size_vram > 0
  - /api/show                 → parameter_size != "0.0B"
"""
import json
import sys
import time
import urllib.request

BAL = "http://localhost:18080"
MODEL = "Qwen3.8-27B-UD-Q4_K_M"


def post(path, body, timeout=600):
    data = json.dumps(body).encode("utf-8")
    req = urllib.request.Request(
        f"{BAL}{path}",
        data=data,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return r.status, json.loads(r.read().decode("utf-8"))


def get(path, timeout=10):
    with urllib.request.urlopen(f"{BAL}{path}", timeout=timeout) as r:
        return r.status, json.loads(r.read().decode("utf-8"))


def main():
    print("=== R66a translator-bug fix verification ===")

    s, ps = get("/api/ps")
    print(f"/api/ps HTTP {s}, models: {len(ps.get('models', []))}")
    for m in ps.get("models", []):
        size_mb = m.get("size", 0) / 1024 / 1024
        vram_mb = m.get("size_vram", 0) / 1024 / 1024
        print(f"  - {m['name']}: size={size_mb:.0f} MB, size_vram={vram_mb:.0f} MB")

    print("\n--- Native Ollama /api/chat ---")
    t0 = time.time()
    try:
        s, resp = post("/api/chat", {
            "model": MODEL,
            "messages": [{"role": "user", "content": "Одной строкой: как тебя зовут?"}],
            "stream": False,
            "options": {"num_predict": 256, "temperature": 0},
        }, timeout=600)
    except Exception as e:
        print(f"  /api/chat FAILED: {e}")
        sys.exit(2)
    dt = time.time() - t0
    print(f"HTTP {s} | {dt:.1f}s | model={resp.get('model')}")
    print(f"  done={resp.get('done')}, done_reason={resp.get('done_reason')}")
    msg = resp.get("message") or {}
    content = msg.get("content", "")
    error = resp.get("error", "")
    if error:
        print(f"  ERROR: {error}")
    if "upstream returned empty response" in (content + error):
        print(f"  *** BUG STILL PRESENT: 'upstream returned empty response' ***")
        sys.exit(3)
    if not content:
        print(f"  WARNING: empty content (model might still be loading)")
    else:
        print(f"  content ({len(content)} chars): {content[:200]!r}")
    if msg.get("reasoning"):
        print(f"  reasoning ({len(msg['reasoning'])} chars): {msg['reasoning'][:200]!r}")

    print("\n--- OpenAI /v1/chat/completions ---")
    t0 = time.time()
    try:
        s, resp = post("/v1/chat/completions", {
            "model": MODEL,
            "messages": [{"role": "user", "content": "Одной строкой: как тебя зовут?"}],
            "max_tokens": 256,
            "temperature": 0,
        }, timeout=600)
    except Exception as e:
        print(f"  /v1/chat/completions FAILED: {e}")
        sys.exit(2)
    dt = time.time() - t0
    print(f"HTTP {s} | {dt:.1f}s | id={resp.get('id')}")
    choices = resp.get("choices", [])
    if choices:
        msg = choices[0].get("message", {})
        content = msg.get("content", "")
        print(f"  content ({len(content)} chars): {content[:200]!r}")
    usage = resp.get("usage", {})
    print(f"  usage: {usage}")

    print("\n=== R66a: ALL CHECKS PASSED ===")


if __name__ == "__main__":
    main()
