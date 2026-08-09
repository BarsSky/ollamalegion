"""
abort_prompt_gen_test.py — Separate prompt-phase and gen-phase cancel latency.

Strategy:
- "prompt phase cancel": cancel BEFORE first chunk (during prompt decode)
- "gen phase cancel": cancel AFTER first chunk (during gen decode)
"""
import http.client
import json
import time

URL_HOST = "localhost"
URL_PORT = 18095
AUTH = "Bearer test-token"
MODEL = "Qwen3-Instruct-2507-q4km"

LONG_PROMPT = (
    "Write a 5000 word essay about machine learning, neural networks, transformers, "
    "attention mechanisms, training procedures, evaluation metrics, ethical considerations, "
    "future directions, and historical context. Be extremely detailed and verbose."
)


def stream_until_cancel(prompt, max_tokens, wait_for_first_chunk_then_cancel_extra=None):
    """
    Send streaming chat.
    - If wait_for_first_chunk_then_cancel_extra is None: cancel immediately
    - Else: wait for first chunk, then cancel after additional seconds
    Returns (chunks, content_len, total_elapsed, time_to_first_chunk, status).
    """
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
    first_chunk_at = None
    cancel_at = None
    try:
        for line in resp:
            if line.startswith(b"data: ") and b"[DONE]" not in line:
                if first_chunk_at is None:
                    first_chunk_at = time.time() - start
                chunks += 1
                try:
                    j = json.loads(line[6:])
                    delta = j.get("choices", [{}])[0].get("delta", {})
                    content_len += len(delta.get("content", ""))
                except Exception:
                    pass
                # Cancel trigger
                if wait_for_first_chunk_then_cancel_extra is not None and first_chunk_at is not None:
                    if time.time() - start > first_chunk_at + wait_for_first_chunk_then_cancel_extra:
                        cancel_at = time.time() - start
                        conn.close()
                        return chunks, content_len, time.time() - start, first_chunk_at, cancel_at, "cancelled"
                # If no first-chunk wait, cancel after first chunk
                elif first_chunk_at is not None:
                    cancel_at = time.time() - start
                    conn.close()
                    return chunks, content_len, time.time() - start, first_chunk_at, cancel_at, "cancelled"
        return chunks, content_len, time.time() - start, first_chunk_at, cancel_at, "finished"
    except Exception as e:
        return chunks, content_len, time.time() - start, first_chunk_at, cancel_at, f"error: {e}"


def main():
    # Warmup
    print("=== Warmup ===")
    for i in range(2):
        conn = http.client.HTTPConnection(URL_HOST, URL_PORT, timeout=30)
        body = json.dumps({"model": MODEL, "messages": [{"role": "user", "content": f"hi {i}"}], "stream": False, "max_tokens": 5})
        conn.request("POST", "/v1/chat/completions", body=body, headers={"Authorization": AUTH, "Content-Type": "application/json"})
        resp = conn.getresponse()
        resp.read()
        conn.close()
    print("  warmup done")

    print()
    print("=== Test A: cancel right after first chunk (gen phase, ~100ms in) ===")
    for i in range(5):
        c, l, total, first_chunk, cancel_at, status = stream_until_cancel(LONG_PROMPT, 5000, wait_for_first_chunk_then_cancel_extra=0.1)
        if first_chunk and cancel_at:
            gen_phase_latency = (cancel_at - first_chunk) * 1000
            print(f"  test {i}: first_chunk={first_chunk*1000:.0f}ms, cancel_at={cancel_at*1000:.0f}ms, gen_latency={gen_phase_latency:.0f}ms, chunks={c}, status={status}")
        else:
            print(f"  test {i}: total={total:.3f}s, first_chunk={first_chunk}, status={status}")

    print()
    print("=== Test B: cancel during prompt phase (before first chunk) ===")
    for i in range(5):
        # Use a custom conn to close early
        import socket
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.connect((URL_HOST, URL_PORT))
        body = json.dumps({
            "model": MODEL,
            "messages": [{"role": "user", "content": LONG_PROMPT}],
            "stream": True,
            "max_tokens": 5000,
        }).encode("utf-8")
        req = b"POST /v1/chat/completions HTTP/1.1\r\n"
        req += f"Host: {URL_HOST}:{URL_PORT}\r\n".encode()
        req += b"Authorization: Bearer test-token\r\n"
        req += b"Content-Type: application/json\r\n"
        req += f"Content-Length: {len(body)}\r\n".encode()
        req += b"Connection: close\r\n"
        req += b"\r\n"
        start = time.time()
        s.sendall(req + body)
        # Read 1 byte (trigger cancel_after=0)
        try:
            data = s.recv(1)
        except Exception:
            pass
        # Immediately close — cancel via TCP reset
        s.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack('ii', 1, 0))
        s.close()
        elapsed = time.time() - start
        print(f"  test {i}: elapsed={elapsed*1000:.0f}ms (cancelled via TCP RST immediately)")


if __name__ == "__main__":
    import struct
    main()
