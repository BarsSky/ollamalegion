#!/usr/bin/env python3
"""R60.44 — comprehensive test of all paths OpenWebUI might use."""
import requests

# Different ways OpenWebUI might be configured to talk to our stack
endpoints = [
    # Direct paths
    "http://localhost:18080/api/chat",
    "http://localhost:18080/v1/chat/completions",
    "http://localhost:18080/api/generate",
    "http://localhost:18080/v1/completions",
    "http://localhost:18080/v1/models",
    "http://localhost:18080/api/tags",
    # Through nginx (webui)
    "http://localhost:18083/api/chat",
    "http://localhost:18083/v1/chat/completions",
    "http://localhost:18083/ollama/api/chat",
    "http://localhost:18083/openai/v1/chat/completions",
    "http://localhost:18083/api/tags",
    "http://localhost:18083/v1/models",
]

# Ollama format
ollama_body = {
    "model": "Qwen3-Instruct-2507-q4km",
    "messages": [{"role": "user", "content": "Привет"}],
    "stream": False,
    "options": {"num_ctx": 4096, "num_predict": 32, "temperature": 0},
}

# OpenAI format
openai_body = {
    "model": "Qwen3-Instruct-2507-q4km",
    "messages": [{"role": "user", "content": "Привет"}],
    "stream": False,
    "max_tokens": 32,
    "temperature": 0,
}

# Ollama tags body (no body needed)
tags_body = None

# OpenAI models (no body needed)

def test(url, body, headers, label):
    try:
        if body:
            r = requests.post(url, json=body, timeout=20, headers=headers)
        else:
            r = requests.get(url, timeout=10, headers=headers)
        first = r.text[:1] if r.text else ""
        is_html = first == "<"
        return {
            "url": url,
            "label": label,
            "http": r.status_code,
            "elapsed": f"{r.elapsed.total_seconds():.1f}s",
            "content_type": r.headers.get("Content-Type"),
            "body_len": len(r.text),
            "is_html": is_html,
            "is_json": first in "{[",
            "snippet": r.text[:120].replace("\n", " "),
        }
    except Exception as e:
        return {"url": url, "label": label, "error": str(e)}

print("=" * 80)
print("R60.44 — exhaustive path test")
print("=" * 80)

results = []
for url in endpoints:
    if "/v1/models" in url or "/api/tags" in url:
        # GET request
        results.append(test(url, None, {"Authorization": "Bearer bundled-default"}, "ollama-get"))
    elif "/v1/chat" in url or "/openai/v1" in url:
        # OpenAI format
        results.append(test(url, openai_body, {"Authorization": "Bearer bundled-default"}, "openai"))
    else:
        # Ollama format
        results.append(test(url, ollama_body, {"Authorization": "Bearer bundled-default"}, "ollama"))

print()
ok = 0
fail = 0
for r in results:
    if "error" in r:
        print(f"❌ {r['label']:10s} {r['url']}")
        print(f"      ERROR: {r['error']}")
        fail += 1
    else:
        status = "✅" if r["http"] == 200 else "❌"
        html = "HTML!" if r["is_html"] else "JSON"
        if r["http"] == 200:
            ok += 1
        else:
            fail += 1
        print(f"{status} {r['label']:10s} {r['url']}")
        print(f"      HTTP {r['http']} ({r['elapsed']}) {html} body_len={r['body_len']}")
        if r["is_html"]:
            print(f"      snippet: {r['snippet']}")
        elif r["http"] != 200:
            print(f"      body: {r['snippet']}")

print()
print(f"Summary: {ok} OK, {fail} FAIL")
