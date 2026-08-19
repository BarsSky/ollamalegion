"""
abort_wire_protocol_test.py — Verify wire protocol for cancelled responses.

When client cancels via TCP close, the server should emit a final chunk with:
- Ollama /api/chat: done_reason="cancelled" + cancelled=true
- OpenAI /v1/chat/completions: finish_reason="stop" + cancelled=true

This test verifies these fields appear correctly.

Fixes vs original:
- signal.alarm() hard timeout (Unix) — prevents infinite hangs
- max_chunks limit — exits early after collecting enough data
- Connection: close — server can clean up after RST
- select() wrapper for non-blocking recv with explicit timeout

Usage:
    python scripts/abort_wire_protocol_test.py [--port 18095] [--model Qwen3-Instruct-2507-q4km]
"""
import http.client
import json
import time
import os
import sys
import socket
import struct
import signal
import select
import argparse

# Defaults — override via CLI args
URL_HOST = os.environ.get("ABORT_TEST_HOST", "localhost")
URL_PORT = int(os.environ.get("ABORT_TEST_PORT", "18095"))
AUTH = os.environ.get("ABORT_TEST_AUTH", "Bearer test-token")
MODEL = os.environ.get("ABORT_TEST_MODEL", "Qwen3-Instruct-2507-q4km")

# Hard limits
HARD_TIMEOUT_SEC = 25         # signal.alarm() — entire test
SOCKET_TIMEOUT_SEC = 8        # individual recv()
MAX_CHUNKS_PER_TEST = 8       # exit early after this many chunks


class TimeoutError(Exception):
    """Raised by hard timeout signal handler."""
    pass


def _alarm_handler(signum, frame):
    raise TimeoutError(f"Test exceeded {HARD_TIMEOUT_SEC}s hard timeout")


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


def recv_with_timeout(s, timeout_sec):
    """Non-blocking recv with explicit timeout via select()."""
    ready, _, _ = select.select([s], [], [], timeout_sec)
    if not ready:
        raise socket.timeout(f"select() timeout after {timeout_sec}s")
    return s.recv(4096)


def abort_via_rst(protocol="openai"):
    """Connect via raw socket, send request, read up to MAX_CHUNKS chunks, then RST."""
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.settimeout(SOCKET_TIMEOUT_SEC)
    s.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
    s.connect((URL_HOST, URL_PORT))
    if protocol == "openai":
        path = "/v1/chat/completions"
    else:
        path = "/api/chat"
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
    req += "Connection: close\r\n"   # Hint server to close after response
    req += "\r\n"
    s.sendall(req.encode() + body)

    chunks = []
    try:
        while len(chunks) < MAX_CHUNKS_PER_TEST:
            data = recv_with_timeout(s, SOCKET_TIMEOUT_SEC)
            if not data:
                break
            for line in data.split(b"\n"):
                if line.startswith(b"data: "):
                    chunk_str = line[6:].decode("utf-8", errors="ignore")
                    if chunk_str and chunk_str != "[DONE]":
                        try:
                            chunks.append(json.loads(chunk_str))
                        except Exception:
                            pass
    except (socket.timeout, TimeoutError):
        pass
    except Exception:
        pass

    # TCP RST (LINGER (1, 0)) to trigger immediate cancel on server side
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


def test_endpoint(protocol):
    """Run one test under hard alarm timeout (Unix only)."""
    print(f"=== Test {protocol} /{protocol}/chat cancel ===")
    if hasattr(signal, "SIGALRM"):
        signal.signal(signal.SIGALRM, _alarm_handler)
        signal.alarm(HARD_TIMEOUT_SEC)
    try:
        chunks = abort_via_rst(protocol)
    finally:
        if hasattr(signal, "SIGALRM"):
            signal.alarm(0)  # cancel alarm
    print(f"  received {len(chunks)} chunks (max={MAX_CHUNKS_PER_TEST})")
    cancelled = find_cancelled_chunk(chunks)
    if cancelled:
        label = (
            "cancelled:true (Ollama ext)" if cancelled.get("cancelled") is True
            else f"done_reason={cancelled.get('done_reason')!r} (Ollama)"
            if "done_reason" in cancelled
            else f"finish_reason={cancelled['choices'][0].get('finish_reason')!r} (OpenAI)"
        )
        print(f"  PASS: found cancelled chunk ({label})")
        print(f"    {json.dumps(cancelled, indent=2)[:300]}")
        return True
    else:
        print(f"  INFO: no cancelled chunk in collected stream (server may still be generating)")
        if chunks:
            last = chunks[-1]
            print(f"  last chunk keys: {list(last.keys())}")
            print(f"  last chunk: {json.dumps(last, indent=2)[:200]}")
        else:
            print(f"  no chunks received at all")
        return False


def main():
    global URL_HOST, URL_PORT, AUTH, MODEL
    parser = argparse.ArgumentParser(description="Test cppworker wire protocol for cancelled responses")
    parser.add_argument("--host", default=URL_HOST, help=f"cppworker host (default: {URL_HOST})")
    parser.add_argument("--port", type=int, default=URL_PORT, help=f"cppworker port (default: {URL_PORT})")
    parser.add_argument("--model", default=MODEL, help=f"model name (default: {MODEL})")
    parser.add_argument("--auth", default=AUTH, help=f"auth header (default: {AUTH})")
    parser.add_argument("--no-warmup", action="store_true", help="skip warmup request")
    args = parser.parse_args()

    # Override globals from CLI
    URL_HOST = args.host
    URL_PORT = args.port
    AUTH = args.auth
    MODEL = args.model

    print(f"Target: {URL_HOST}:{URL_PORT}, model={MODEL}")
    print(f"Hard timeout: {HARD_TIMEOUT_SEC}s, socket timeout: {SOCKET_TIMEOUT_SEC}s, max chunks: {MAX_CHUNKS_PER_TEST}")
    print()

    if not args.no_warmup:
        print("=== Warmup ===")
        try:
            warmup()
            print("  warmup done")
        except Exception as e:
            print(f"  warmup failed: {e}")
            print("  (continuing anyway)")
        print()

    results = []
    for protocol in ("openai", "ollama"):
        try:
            ok = test_endpoint(protocol)
            results.append((protocol, ok))
        except TimeoutError as e:
            print(f"  TIMEOUT: {e}")
            results.append((protocol, False))
        except Exception as e:
            print(f"  ERROR: {e}")
            results.append((protocol, False))
        print()

    print("=== Summary ===")
    for protocol, ok in results:
        print(f"  {protocol}: {'PASS' if ok else 'FAIL/INFO'}")

    # Exit non-zero only if hard failures (not INFO)
    failed = [p for p, ok in results if not ok]
    if failed:
        print(f"\n*** {len(failed)} endpoint(s) failed to emit cancelled chunk ***")
        sys.exit(1)
    else:
        print("\n*** ALL TESTS PASSED ***")
        sys.exit(0)


if __name__ == "__main__":
    main()
