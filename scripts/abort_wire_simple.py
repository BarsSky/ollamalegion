"""
abort_wire_simple.py — Simple wire protocol test.

Sends a request, reads chunks until 2-3 SSE events, then RST-closes.
Returns the chunks it collected (no infinite recv).
"""
import http.client
import json
import time
import socket
import struct

URL_HOST = "localhost"
URL_PORT = 18095
AUTH = "Bearer test-token"
MODEL = "Qwen3-Instruct-2507-q4km"


def test_endpoint(path):
    """Test one endpoint with RST-cancel."""
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.connect((URL_HOST, URL_PORT))
    s.settimeout(20)
    body = json.dumps({
        "model": MODEL,
        "messages": [{"role": "user", "content": "Write a long detailed essay about the industrial revolution including steam engines, factories, social changes, child labor, and the transition to modern economies."}],
        "stream": True,
        "max_tokens": 5000,
    }).encode("utf-8")
    req = f"POST {path} HTTP/1.1\r\n"
    req += f"Host: {URL_HOST}:{URL_PORT}\r\n"
    req += f"Authorization: {AUTH}\r\n"
    req += "Content-Type: application/json\r\n"
    req += f"Content-Length: {len(body)}\r\n"
    req += "Connection: close\r\n\r\n"
    s.sendall(req.encode() + body)

    chunks = []
    try:
        while len(chunks) < 5:
            data = s.recv(8192)
            if not data:
                break
            for line in data.split(b"\n"):
                if line.startswith(b"data: "):
                    cs = line[6:].decode("utf-8", errors="ignore")
                    if cs and cs != "[DONE]":
                        try:
                            chunks.append(json.loads(cs))
                        except Exception:
                            pass
    except socket.timeout:
        pass

    # TCP RST to trigger cancel
    try:
        s.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack('ii', 1, 0))
        s.close()
    except Exception:
        pass

    return chunks


def find_cancelled(chunks):
    """Look for a chunk signalling cancel."""
    for c in chunks:
        # Top-level cancelled
        if c.get("cancelled") is True:
            return ("cancelled:true", c)
        # Ollama done_reason
        if c.get("done_reason") == "cancelled":
            return ("done_reason:cancelled", c)
        # OpenAI finish_reason
        for choice in c.get("choices", []):
            if choice.get("cancelled") is True:
                return ("choice.cancelled:true", c)
            if choice.get("finish_reason") == "cancelled":
                return ("choice.finish_reason:cancelled", c)
    return (None, None)


def main():
    print("=== Test OpenAI /v1/chat/completions ===")
    chunks = test_endpoint("/v1/chat/completions")
    print(f"  received {len(chunks)} chunks before RST")
    label, c = find_cancelled(chunks)
    if c:
        print(f"  PASS: {label}")
        print(f"    {json.dumps(c, indent=2)[:300]}")
    else:
        print(f"  Note: no cancelled chunk in stream (server still emitting data)")
        if chunks:
            print(f"  last chunk: {json.dumps(chunks[-1], indent=2)[:200]}")

    print()
    print("=== Test Ollama /api/chat ===")
    chunks = test_endpoint("/api/chat")
    print(f"  received {len(chunks)} chunks before RST")
    label, c = find_cancelled(chunks)
    if c:
        print(f"  PASS: {label}")
        print(f"    {json.dumps(c, indent=2)[:300]}")
    else:
        print(f"  Note: no cancelled chunk in stream (server still emitting data)")
        if chunks:
            print(f"  last chunk: {json.dumps(chunks[-1], indent=2)[:200]}")


if __name__ == "__main__":
    main()
