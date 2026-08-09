"""
abort_warm_test.py — Measure gen-phase cancel latency (after warmup).

Strategy:
1. Send a few warmup requests to warm up CUDA kernels
2. Then measure cancel latency — should drop to <200ms (gen phase, not first batch)
"""
import http.client
import json
import time
import os

URL_HOST = "localhost"
URL_PORT = 18095
AUTH = "Bearer test-token"
MODEL = "Qwen3-Instruct-2507-q4km"

def stream_chat(prompt, max_tokens, cancel_at_seconds=None):
    """Send streaming chat, optionally cancel."""
    conn = http.client.HTTPConnection(URL_HOST, URL_PORT, timeout=60)
    body = json.dumps({
        "model": MODEL,
        "messages": [{"role": "user", "content": prompt}],
        "stream": True,
        "max_tokens": max_tokens,
    })
    headers = {"Authorization": AUTH, "Content-Type": "application/json"}
    start = time.time()
    conn.request("POST", "/v1/chat/completions", body=body, headers=headers)
    resp = conn.getresponse()
    chunks = 0
    content_len = 0
    try:
        for line in resp:
            if line.startswith(b"data: ") and b"[DONE]" not in line:
                chunks += 1
                try:
                    j = json.loads(line[6:])
                    delta = j.get("choices", [{}])[0].get("delta", {})
                    content_len += len(delta.get("content", ""))
                except Exception:
                    pass
                if cancel_at_seconds and time.time() - start > cancel_at_seconds:
                    elapsed = time.time() - start
                    conn.close()
                    return chunks, content_len, elapsed, "cancelled"
        return chunks, content_len, time.time() - start, "finished"
    except Exception as e:
        return chunks, content_len, time.time() - start, f"error: {e}"


def main():
    # Warmup: 3 short requests
    print("=== Warmup (3 short requests to warm CUDA kernels) ===")
    for i in range(3):
        c, l, e, s = stream_chat(f"hi {i}", 5, None)
        print(f"  warmup {i}: chunks={c}, elapsed={e:.3f}s, status={s}")

    print()
    print("=== Gen-phase cancel latency (after warmup, cancel after 200ms) ===")
    # Use a long prompt + high max_tokens so the gen phase is long
    long_prompt = "Write a 5000 word essay about machine learning, neural networks, transformers, attention mechanisms, training procedures, evaluation metrics, ethical considerations, future directions, and historical context. Be extremely detailed and verbose."
    cancel_at = 0.2
    for i in range(5):
        c, l, e, s = stream_chat(long_prompt, 5000, cancel_at)
        # The cancel latency = e - cancel_at (time from cancel signal to TCP close)
        # If e ≈ cancel_at, cancel latency is 0 (ideal)
        # If e > cancel_at, that's the time it took for C-bridge to return after abort
        latency = max(0, e - cancel_at)
        print(f"  test {i}: chunks={c}, content_len={l}, total_elapsed={e:.3f}s, status={s}, cancel_latency≈{latency*1000:.0f}ms")

    print()
    print("=== Worst case: cancel after first chunk (50ms) ===")
    for i in range(5):
        c, l, e, s = stream_chat(long_prompt, 5000, 0.05)
        latency = max(0, e - 0.05)
        print(f"  test {i}: chunks={c}, total_elapsed={e:.3f}s, status={s}, cancel_latency≈{latency*1000:.0f}ms")


if __name__ == "__main__":
    main()
