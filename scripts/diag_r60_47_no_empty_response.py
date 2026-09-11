#!/usr/bin/env python3
"""
diag_r60_47_no_empty_response.py — R60.47 regression test.

Symptom (pre-R60.47): user reports "empty response" from balancer.
Root cause: handleNCtxReloadActual делал sync DoReload с
`?wait=true&waitTimeoutSec=300` — блокировал HTTP handler до 5 мин.
OpenWebUI/Cline timeout 30-60s → client cancel → STATUS:000 SIZE:0b.

Post-R60.47:
  1. R60.47b: ResolveNumCtx Tier 3 upgrades backend_default → loaded_n_ctx
     when loaded > default. Предотвращает unnecessary 400 от cppworker.
  2. R60.47: async n_ctx reload. Если 400 всё-таки случается — balancer
     returns 503+Retry-After+digestive за <50ms. Client retry-ит через
     Retry-After → reload завершён → 200 OK.

Test scenarios:
  1. /api/chat non-stream (был hanging 90s empty body) — теперь 200 за 2-5s
  2. /v1/chat/completions non-stream — теперь 200 за 2-5s
  3. /v1/chat/completions streaming — теперь работает
  4. Все endpoints возвращают НЕ empty body

Запуск: python scripts/diag_r60_47_no_empty_response.py [host] [port]
"""
import json
import sys
import time
import urllib.request
import urllib.error

DEFAULT_HOST = "http://localhost:18080"
AUTH = "Bearer bundled-default"

# Test request body (без num_ctx — OpenWebUI default)
REQ_BODY = json.dumps({
    "model": "Qwen3-Instruct-2507-q4km",
    "messages": [{"role": "user", "content": "Say hi"}],
    "stream": False,
    "max_tokens": 16,
}).encode("utf-8")


def make_request(host, path, body, stream=False, timeout=30):
    """Send request, return (status_code, body_bytes, elapsed_seconds)."""
    url = f"{host}{path}"
    headers = {
        "Authorization": AUTH,
        "Content-Type": "application/json",
    }
    if stream:
        headers["Accept"] = "text/event-stream"

    req = urllib.request.Request(url, data=body, headers=headers, method="POST")
    start = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body_bytes = resp.read()
            status = resp.status
    except urllib.error.HTTPError as e:
        # Even on error we want to see the body (actionable diagnostics)
        body_bytes = e.read()
        status = e.code
    except Exception as e:
        return None, str(e).encode(), time.time() - start
    elapsed = time.time() - start
    return status, body_bytes, elapsed


def test_endpoint(name, host, path, body, stream=False, timeout=30):
    """Test one endpoint, print result, return True if PASS."""
    print(f"\n=== {name} ({path}) ===")
    status, body_bytes, elapsed = make_request(host, path, body, stream=stream, timeout=timeout)
    body_str = body_bytes.decode("utf-8", errors="replace") if isinstance(body_bytes, bytes) else str(body_bytes)
    body_len = len(body_bytes) if isinstance(body_bytes, bytes) else 0

    print(f"  Status: {status}")
    print(f"  Elapsed: {elapsed:.2f}s")
    print(f"  Body length: {body_len}b")

    # Pre-R60.47 failure mode: status==None (timeout), body_len==0
    if status is None:
        print(f"  ❌ FAIL: Connection failed/timed out (pre-R60.47 bug)")
        return False

    # Empty body = the user-reported symptom
    if body_len == 0:
        print(f"  ❌ FAIL: Empty body (pre-R60.47 bug)")
        return False

    # Status 503 with JSON body = async reload triggered (acceptable)
    if status == 503:
        # Should have Retry-After header + JSON body with diagnostics
        try:
            body_json = json.loads(body_str)
            has_error = "error" in body_json
            has_retry = "retry_after" in body_json or "decision" in body_json
            print(f"  Body: {body_str[:200]}")
            if has_error and has_retry:
                print(f"  ✅ PASS: 503 with actionable diagnostics (R60.47 async mode)")
                return True
            else:
                print(f"  ⚠️  PARTIAL: 503 but missing actionable fields")
                return False
        except json.JSONDecodeError:
            print(f"  ❌ FAIL: 503 but body is not JSON")
            return False

    # Status 200 — full success
    if status == 200:
        # Try to parse JSON (or SSE for streaming)
        if stream:
            # SSE: should have data: lines
            data_lines = [line for line in body_str.split("\n") if line.startswith("data: ")]
            print(f"  SSE data lines: {len(data_lines)}")
            if len(data_lines) > 0:
                print(f"  ✅ PASS: 200 OK with {len(data_lines)} SSE chunks")
                return True
            else:
                print(f"  ❌ FAIL: 200 but no SSE data")
                return False
        else:
            try:
                body_json = json.loads(body_str)
                if "choices" in body_json or "message" in body_json or "response" in body_json:
                    content_preview = ""
                    if "choices" in body_json and body_json["choices"]:
                        msg = body_json["choices"][0].get("message", {})
                        content_preview = msg.get("content", "")[:50]
                    elif "message" in body_json:
                        content_preview = body_json["message"].get("content", "")[:50]
                    elif "response" in body_json:
                        content_preview = str(body_json["response"])[:50]
                    print(f"  Content preview: {content_preview!r}")
                    print(f"  ✅ PASS: 200 OK with model response")
                    return True
                else:
                    print(f"  ❌ FAIL: 200 but no content field")
                    print(f"  Body: {body_str[:200]}")
                    return False
            except json.JSONDecodeError:
                print(f"  ❌ FAIL: 200 but body is not JSON")
                print(f"  Body: {body_str[:200]}")
                return False

    print(f"  ⚠️  UNEXPECTED: status={status}")
    return False


def main():
    host = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_HOST
    print(f"R60.47 Regression Test")
    print(f"======================")
    print(f"Target: {host}")
    print(f"Auth: {AUTH}")
    print(f"Pre-R60.47: /api/chat hangs 90s with empty body (STATUS:000)")
    print(f"Post-R60.47: /api/chat returns 200 with content in 2-5s")

    results = []

    # Test 1: /api/chat non-stream (THE main bug — was hanging 90s)
    results.append(("API_CHAT_NON_STREAM", test_endpoint(
        "/api/chat non-streaming", host, "/api/chat", REQ_BODY, stream=False, timeout=15)))

    # Test 2: /v1/chat/completions non-stream
    body_no_stream = json.dumps({
        "model": "Qwen3-Instruct-2507-q4km",
        "messages": [{"role": "user", "content": "Say hi"}],
        "max_tokens": 16,
    }).encode("utf-8")
    results.append(("OPENAI_CHAT_NON_STREAM", test_endpoint(
        "/v1/chat/completions non-stream", host, "/v1/chat/completions", body_no_stream, stream=False, timeout=15)))

    # Test 3: /v1/chat/completions streaming
    body_stream = json.dumps({
        "model": "Qwen3-Instruct-2507-q4km",
        "messages": [{"role": "user", "content": "Say hi"}],
        "max_tokens": 16,
        "stream": True,
    }).encode("utf-8")
    results.append(("OPENAI_CHAT_STREAMING", test_endpoint(
        "/v1/chat/completions streaming", host, "/v1/chat/completions", body_stream, stream=True, timeout=15)))

    # Summary
    print("\n" + "=" * 50)
    print("SUMMARY")
    print("=" * 50)
    total = len(results)
    passed = sum(1 for _, ok in results if ok)
    for name, ok in results:
        marker = "✅" if ok else "❌"
        print(f"  {marker} {name}")

    print(f"\nResult: {passed}/{total} PASS")
    if passed == total:
        print("🎉 R60.47 fix verified — no more empty responses!")
        sys.exit(0)
    else:
        print("❌ Some tests failed")
        sys.exit(1)


if __name__ == "__main__":
    main()
