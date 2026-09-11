#!/usr/bin/env python3
"""R60.43 — reproduce 'immediately empty response' the user is reporting."""
import requests
import time

NGINX = "http://localhost:18083"
body = {
    "model": "Qwen3-Instruct-2507-q4km",
    "messages": [{"role": "user", "content": "Привет, распиши сайт"}],
    "stream": False,
    "options": {"num_ctx": 4096, "num_predict": 256, "temperature": 0.5},
}

# Multiple rapid requests to see what the user sees
for attempt in range(3):
    start = time.time()
    try:
        r = requests.post(
            f"{NGINX}/api/chat",
            json=body,
            timeout=10,
            headers={"Authorization": "Bearer changeme-bundled-with-agent-token"},
        )
        elapsed = time.time() - start
        body_text = r.text
        first_char = body_text[:1] if body_text else ""
        is_html = first_char == "<"
        is_json = first_char in "{["
        print(f"Attempt {attempt+1}: HTTP {r.status_code} in {elapsed:.1f}s, body_len={len(body_text)}, is_html={is_html}, is_json={is_json}")
        print(f"  Retry-After: {r.headers.get('Retry-After')}")
        if body_text and not is_json:
            print(f"  body: {body_text[:200]}")
        elif body_text:
            try:
                d = r.json()
                if "error" in d:
                    print(f"  error: {d['error'][:200]}")
                if "done" in d:
                    print(f"  done: {d['done']}, eval_count: {d.get('eval_count')}")
                if "message" in d and isinstance(d["message"], dict):
                    print(f"  content: {d['message'].get('content', '')[:100]!r}")
            except Exception as e:
                print(f"  parse err: {e}")
    except requests.exceptions.Timeout:
        print(f"Attempt {attempt+1}: TIMEOUT after 10s")
    except Exception as e:
        print(f"Attempt {attempt+1}: EXCEPTION: {e}")
    time.sleep(2)
