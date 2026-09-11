#!/usr/bin/env python3
"""R60.42 — reproduce user scenario with single request + poll + retry."""
import requests
import time
import json

NGINX = "http://localhost:18083"
MODEL = "Qwen3-Instruct-2507-q4km"
PROMPT = "Привет распиши красивый сайт на html css для интерактивной математики расчета движения полета"

def show(label, r):
    print(f"--- {label} ---")
    print(f"  HTTP {r.status_code} in {r.elapsed.total_seconds():.1f}s")
    print(f"  Retry-After: {r.headers.get('Retry-After')}")
    if r.text:
        try:
            d = r.json()
            if "error" in d:
                print(f"  error: {d['error'][:150]}")
            if "target_n_ctx" in d:
                print(f"  target_n_ctx: {d['target_n_ctx']}")
            if "suggestion" in d:
                print(f"  suggestion: {d['suggestion'][:150]}")
            if "message" in d and isinstance(d["message"], dict):
                content = d["message"].get("content", "")
                if content:
                    print(f"  content_len: {len(content)}")
                    print(f"  content[:80]: {content[:80]!r}")
            if "done_reason" in d:
                print(f"  done_reason: {d['done_reason']}")
            if "eval_count" in d:
                print(f"  eval_count: {d['eval_count']}")
        except Exception:
            print(f"  body[:200]: {r.text[:200]}")

# Request 1
print("\n=== Request 1: User clicks Send ===")
body = {
    "model": MODEL,
    "messages": [{"role": "user", "content": PROMPT}],
    "stream": False,
    "options": {"num_ctx": 4096, "num_predict": 1024, "temperature": 0.5},
}
r1 = requests.post(
    f"{NGINX}/api/chat",
    json=body,
    timeout=10,
    headers={"Authorization": "Bearer changeme-bundled-with-agent-token"},
)
show("Request 1 result", r1)

# Poll cppworker
print("\n=== Polling cppworker for load completion (max 240s) ===")
start = time.time()
loaded = False
while time.time() - start < 240:
    time.sleep(5)
    elapsed = time.time() - start
    try:
        cr = requests.get("http://localhost:18092/api/models", timeout=3)
        s = cr.json()
        if s.get("count") == 1 and s["models"][0].get("state") == "loaded":
            m = s["models"][0]
            active = m.get("active_queries", 0)
            print(f"  T+{elapsed:.0f}s: LOADED, ctx={m.get('context_size')}, active={active}")
            if active == 0:
                loaded = True
                break
        else:
            st = s["models"][0].get("state") if s.get("count") == 1 else "?"
            print(f"  T+{elapsed:.0f}s: count={s.get('count')}, state={st}")
    except Exception as e:
        print(f"  T+{elapsed:.0f}s: err {e}")

if not loaded:
    print("Model never loaded after 240s polling")
    # Show what cppworker reports
    try:
        s = requests.get("http://localhost:18092/api/models", timeout=3).json()
        print(f"Final cppworker state: {json.dumps(s, indent=2)[:500]}")
    except Exception:
        pass
    raise SystemExit(1)

# Request 2 (now model should be ready)
print("\n=== Request 2: After load complete ===")
r2 = requests.post(
    f"{NGINX}/api/chat",
    json=body,
    timeout=180,
    headers={"Authorization": "Bearer changeme-bundled-with-agent-token"},
)
show("Request 2 result", r2)
