"""
abort_latency_test.py — Measure abort API cancel latency with real C-bridge.

Sends long streaming requests, cancels mid-stream, measures:
1. Time from cancel to TCP close (should be < 100ms = single batch time)
2. Whether the model is still usable after abort
3. Concurrent aborts work correctly
"""
import http.client
import json
import time
import threading
import os

URL_HOST = "localhost"
URL_PORT = 18095
AUTH = "Bearer test-token"
MODEL = "Qwen3-Instruct-2507-q4km"

def stream_test(req_id, max_tokens, cancel_at_seconds, prompt=None):
    """Send streaming chat, optionally cancel, return timing info."""
    if prompt is None:
        prompt = (
            f"Request {req_id}: write a very long detailed essay about the history of "
            "human civilization including ancient Egypt, Greece, Rome, medieval Europe, "
            "renaissance, industrial revolution, modern era. Cover major events, "
            "technologies, and philosophical shifts. Be extremely detailed and verbose."
        )
    conn = http.client.HTTPConnection(URL_HOST, URL_PORT, timeout=60)
    body = json.dumps({
        "model": MODEL,
        "messages": [{"role": "user", "content": prompt}],
        "stream": True,
        "max_tokens": max_tokens,
    })
    headers = {"Authorization": AUTH, "Content-Type": "application/json"}
    start = time.time()
    try:
        conn.request("POST", "/v1/chat/completions", body=body, headers=headers)
        resp = conn.getresponse()
        chunks = 0
        total_bytes = 0
        content_len = 0
        for line in resp:
            total_bytes += len(line)
            if line.startswith(b"data: ") and b"[DONE]" not in line:
                chunks += 1
                # extract content length
                try:
                    j = json.loads(line[6:])
                    delta = j.get("choices", [{}])[0].get("delta", {})
                    content = delta.get("content", "")
                    content_len += len(content)
                except Exception:
                    pass
                if cancel_at_seconds and time.time() - start > cancel_at_seconds:
                    cancel_at = time.time() - start
                    conn.close()
                    return req_id, chunks, cancel_at, total_bytes, content_len, "cancelled"
        finished_at = time.time() - start
        return req_id, chunks, finished_at, total_bytes, content_len, "finished"
    except Exception as e:
        return req_id, chunks, time.time() - start, total_bytes, content_len, f"error: {e}"


def main():
    print("=== Test 1: 1 request, cancel after 0.5s ===")
    r = stream_test(1, 8000, 0.5)
    print(f"  req {r[0]}: chunks={r[1]}, cancel_at={r[2]:.3f}s, bytes={r[3]}, content_len={r[4]}, status={r[5]}")

    print()
    print("=== Test 2: 3 concurrent requests, cancel each after 0.5s ===")
    results = []
    def run(req_id):
        results.append(stream_test(req_id, 8000, 0.5))
    threads = [threading.Thread(target=run, args=(i,)) for i in range(3)]
    start = time.time()
    for t in threads: t.start()
    for t in threads: t.join()
    print(f"  total elapsed: {time.time()-start:.2f}s")
    for r in results:
        print(f"  req {r[0]}: chunks={r[1]}, cancel_at={r[2]:.3f}s, bytes={r[3]}, content_len={r[4]}, status={r[5]}")

    print()
    print("=== Test 3: model still works after aborts (short no-cancel request) ===")
    r = stream_test(99, 30, None)  # don't cancel, short
    print(f"  req 99: chunks={r[1]}, elapsed={r[2]:.3f}s, content_len={r[4]}, status={r[5]}")

    print()
    print("=== Test 4: 5 concurrent, cancel immediately after 1st chunk (worst case) ===")
    results = []
    def run(req_id):
        results.append(stream_test(req_id, 8000, 0.05))  # cancel after 50ms
    threads = [threading.Thread(target=run, args=(i,)) for i in range(5)]
    start = time.time()
    for t in threads: t.start()
    for t in threads: t.join()
    print(f"  total elapsed: {time.time()-start:.2f}s")
    for r in results:
        print(f"  req {r[0]}: chunks={r[1]}, cancel_at={r[2]:.3f}s, status={r[5]}")

    print()
    print("=== Test 5: 10 sequential requests with cancel each (stress test) ===")
    for i in range(10):
        r = stream_test(100+i, 5000, 0.3)
        print(f"  req {r[0]}: chunks={r[1]}, cancel_at={r[2]:.3f}s, content_len={r[4]}, status={r[5]}")


if __name__ == "__main__":
    main()
