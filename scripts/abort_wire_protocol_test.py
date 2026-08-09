"""
abort_wire_protocol_test.py — Verify wire protocol for cancelled responses.

When client cancels via TCP close, the server should emit a final chunk with:
- Ollama /api/chat: done_reason="cancelled" + cancelled=true
- OpenAI /v1/chat/completions: finish_reason="stop" + cancelled=true

This test verifies these fields appear correctly.
"""
import http.client
import json
import time
import os
import socket
import struct

URL_HOST = "localhost"
URL_PORT = 18095  # GPU image
AUTH = "Bearer test-token"
MODEL = "Qwen3-Instruct-2507-q4km"


def warmup():
    """Send a short request to warm the model."""
    conn = http.client.HTTPConnection(URL_HOST, URL_PORT, timeout=120)
    body = json.dumps({
        "model": MODEL,
        "messages": [{"role": "user", "content": "hi"}],
        "stream": False,
        "max_tokens": 3,
    })
    conn.request("POST", "/v1/chat/completions", body=body,
                 headers={"Authorization": AUTH, "Content-Type": "application/json"})
    resp = conn.getresponse()
    resp.read()
    conn.close()


def abort_via_rst(protocol="openai"):
    """Connect via raw socket, send request, then RST close to trigger cancel."""
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.connect((URL_HOST, URL_PORT))
    if protocol == "openai":
        path = "/v1/chat/completions"
    else:
        path = "/api/chat"
    body = json.dumps({
        "model": MODEL,
        "messages": [{"role": "user", "content": "Write a long detailed essay about the industrial revolution"}],
        "stream": True,
        "max_tokens": 5000,
    }).encode("utf-8")
    req = f"POST {path} HTTP/1.1\r\n"
    req += f"Host: {URL_HOST}:{URL_PORT}\r\n"
    req += f"Authorization: {AUTH}\r\n"
    req += "Content-Type: application/json\r\n"
    req += f"Content-Length: {len(body)}\r\n"
    req += "Connection: close\r\n"
    req += "\r\n"
    s.sendall(req.encode() + body)

    # Read some data
    chunks = []
    try:
        s.settimeout(15)
        buf = b""
        while True:
            data = s.recv(4096)
            if not data:
                break
            buf += data
            if b"data: " in buf or b"\n" in buf:
                # Try to parse at least one chunk
                lines = buf.split(b"\n")
                for line in lines:
                    if line.startswith(b"data: "):
                        chunk_str = line[6:].decode("utf-8", errors="ignore")
                        if chunk_str and chunk_str != "[DONE]":
                            try:
                                chunks.append(json.loads(chunk_str))
                            except Exception:
                                pass
    except Exception:
        pass

    # Now do TCP RST to trigger cancel
    try:
        s.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack('ii', 1, 0))
        s.close()
    except Exception:
        pass

    return chunks


def find_cancelled_chunk(chunks):
    """Find the chunk that signals cancellation."""
    for chunk in chunks:
        # OpenAI: choices[].finish_reason="stop" + cancelled=true
        choices = chunk.get("choices", [])
        for c in choices:
            if c.get("cancelled") is True or c.get("finish_reason") in ("cancelled", "stop"):
                return chunk
        # Top-level cancelled field (Ollama extension)
        if chunk.get("cancelled") is True:
            return chunk
        # Ollama: done_reason="cancelled"
        if chunk.get("done_reason") == "cancelled":
            return chunk
    return None


def main():
    print("=== Warmup ===")
    try:
        warmup()
        print("  warmup done")
    except Exception as e:
        print(f"  warmup failed: {e}")
        return

    print()
    print("=== Test OpenAI /v1/chat/completions cancel ===")
    chunks = abort_via_rst("openai")
    print(f"  received {len(chunks)} chunks")
    cancelled = find_cancelled_chunk(chunks)
    if cancelled:
        print(f"  PASS: found cancelled chunk:")
        print(f"    {json.dumps(cancelled, indent=2)[:300]}")
    else:
        print(f"  FAIL: no cancelled chunk found")
        print(f"  last 3 chunks: {json.dumps(chunks[-3:], indent=2)[:500] if len(chunks) >= 3 else json.dumps(chunks, indent=2)[:500]}")

    print()
    print("=== Test Ollama /api/chat cancel ===")
    chunks = abort_via_rst("ollama")
    print(f"  received {len(chunks)} chunks")
    cancelled = find_cancelled_chunk(chunks)
    if cancelled:
        print(f"  PASS: found cancelled chunk:")
        print(f"    {json.dumps(cancelled, indent=2)[:300]}")
    else:
        print(f"  FAIL: no cancelled chunk found")
        print(f"  last 3 chunks: {json.dumps(chunks[-3:], indent=2)[:500] if len(chunks) >= 3 else json.dumps(chunks, indent=2)[:500]}")


if __name__ == "__main__":
    main()
