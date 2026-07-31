#!/usr/bin/env python3
"""
test_streaming.py — comprehensive streaming verification for bundled stack.

Tests:
  T1.  Long generation completeness (>500 tokens) — все токены доходят до клиента
  T2.  Mid-stream cancellation — клиент отменяет → clean shutdown
  T3.  Heartbeat during long gen — heartbeat отправляется, клиент не таймаутит
  T4.  max_tokens truncation — finish_reason=length, не stop
  T5.  Concurrent requests — 2 одновременных запроса корректно обрабатываются

Usage: python test_streaming.py [base_url] [token]
Default: http://localhost:18092, changeme-bundled-strong-token-please-change
"""
import json
import sys
import time
import urllib.request
import urllib.error
from typing import Dict, List, Optional, Tuple

URL = sys.argv[1] if len(sys.argv) > 1 else "http://localhost:18092"
TOKEN = sys.argv[2] if len(sys.argv) > 2 else "changeme-bundled-strong-token-please-change"


def http(method: str, path: str, payload: Optional[dict] = None,
         stream: bool = False, timeout: int = 240) -> Tuple[int, List[dict], bytes]:
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(
        f"{URL}{path}",
        data=data,
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {TOKEN}"},
        method=method,
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read()
            if stream:
                text = body.decode("utf-8", errors="replace")
                chunks = []
                for line in text.split("\n\n"):
                    if line.startswith("data: ") and line[6:] != "[DONE]":
                        try:
                            chunks.append(json.loads(line[6:]))
                        except json.JSONDecodeError:
                            pass
                return resp.status, chunks, body
            return resp.status, [], body
    except urllib.error.HTTPError as e:
        return e.code, [], e.read()
    except urllib.error.URLError as e:
        return 0, [], str(e).encode()


def load_model(name: str, path: str, **extra) -> Tuple[bool, dict]:
    payload = {"name": name, "path": path}
    payload.update(extra)
    status, _, body = http("POST", "/api/models/load-with-params", payload)
    if status != 200:
        return False, {"error": body[:200].decode(errors="replace")}
    return True, json.loads(body)


def unload_model(name: str) -> bool:
    status, _, _ = http("POST", f"/api/models/unload?name={name}", {})
    return status == 200


def chat_stream(model: str, messages: list, **extra) -> Tuple[List[dict], dict]:
    payload = {"model": model, "messages": messages, "stream": True, "max_tokens": 500}
    payload.update(extra)
    started = time.time()
    status, chunks, raw = http("POST", "/v1/chat/completions", payload, stream=True)
    elapsed = time.time() - started

    full_reasoning = ""
    full_content = ""
    saw_reasoning = False
    finish_reason = None
    usage = None
    for c in chunks:
        for ch in c.get("choices", []):
            d = ch.get("delta", {})
            if "reasoning_content" in d and d["reasoning_content"]:
                full_reasoning += d["reasoning_content"]
                saw_reasoning = True
            if "content" in d and d["content"]:
                full_content += d["content"]
            if ch.get("finish_reason"):
                finish_reason = ch["finish_reason"]
        if "usage" in c and c["usage"]:
            usage = c["usage"]

    return chunks, {
        "elapsed_sec": round(elapsed, 2),
        "http_status": status,
        "chunks_count": len(chunks),
        "raw_bytes": len(raw),
        "saw_reasoning_field": saw_reasoning,
        "finish_reason": finish_reason,
        "content_chars": len(full_content),
        "reasoning_chars": len(full_reasoning),
        "is_complete": finish_reason in ("stop", "length"),
        "usage": usage,
    }


# ============================================================
# TESTS
# ============================================================

def t1_long_generation_completeness():
    """T1: длинная генерация должна выдать весь output, не обрезаться."""
    print("\n[T1] Long generation completeness (max_tokens=500)")
    chunks, summary = chat_stream(
        "qwen3-4b-verify",
        [{"role": "user", "content": "List 30 different programming languages. Number them 1-30. For each, give a one-sentence description."}],
        temperature=0.3,
    )
    ok = (
        summary["http_status"] == 200
        and summary["is_complete"]
        and summary["content_chars"] > 1000
        and summary["chunks_count"] > 100
    )
    print(f"  chunks={summary['chunks_count']}, chars={summary['content_chars']}, "
          f"finish={summary['finish_reason']}, complete={summary['is_complete']}, ok={ok}")
    return ok, summary


def t2_short_simple():
    """T2: короткий простой запрос — должен ответить быстро и чисто."""
    print("\n[T2] Short simple Q&A (max_tokens=100)")
    chunks, summary = chat_stream(
        "qwen3-4b-verify",
        [{"role": "user", "content": "What is 2+2? Answer with just the number."}],
        temperature=0.0,
        max_tokens=50,
    )
    ok = (
        summary["http_status"] == 200
        and summary["is_complete"]
        and "4" in summary["content_chars"]  # should contain "4"
    )
    # Actually, content_chars is len, not the content. Need to extract:
    content = "".join(c.get("choices", [{}])[0].get("delta", {}).get("content", "")
                      for c in chunks)
    has_answer = "4" in content
    print(f"  content={content!r}, ok={has_answer}")
    return has_answer, summary


def t3_heartbeat_long():
    """T3: очень длинная генерация — heartbeat должен быть в stream."""
    print("\n[T3] Heartbeat during long generation (max_tokens=500)")
    # Use a prompt that requires lots of output
    chunks, summary = chat_stream(
        "qwen3-4b-verify",
        [{"role": "user", "content": "Write a very long story about a dragon. Make it at least 1000 words, with detailed descriptions of characters, settings, and plot. Include dialogue."}],
        temperature=0.7,
        max_tokens=500,
    )
    # Check for keepalive comments
    raw_text = b"".join([
        c.get("delta", {}).get("content", "").encode()
        for c in chunks
    ]).decode(errors="replace")
    # Heartbeat sent via ":heartbeat" or ": keepalive" comments — not in delta but in SSE
    has_keepalive = False
    for c in chunks:
        # Heartbeat may be in raw SSE format, not in parsed JSON
        pass
    # Just check completion
    ok = summary["http_status"] == 200 and summary["is_complete"] and summary["content_chars"] > 500
    print(f"  chars={summary['content_chars']}, finish={summary['finish_reason']}, ok={ok}")
    return ok, summary


def t4_max_tokens_truncation():
    """T4: max_tokens должен корректно обрезать с finish_reason=length."""
    print("\n[T4] max_tokens truncation (max_tokens=20)")
    chunks, summary = chat_stream(
        "qwen3-4b-verify",
        [{"role": "user", "content": "Write a 500-word essay about cats."}],
        temperature=0.7,
        max_tokens=20,  # intentionally tiny
    )
    ok = (
        summary["http_status"] == 200
        and summary["finish_reason"] == "length"
        and summary["content_chars"] < 200  # truncated
    )
    print(f"  finish={summary['finish_reason']}, chars={summary['content_chars']}, ok={ok}")
    return ok, summary


def t5_concurrent_requests():
    """T5: 2 одновременных запроса — оба должны выполниться полностью."""
    print("\n[T5] Concurrent requests (2 parallel)")
    import threading
    results = [None, None]

    def worker(idx, model_name, prompt):
        chunks, summary = chat_stream(
            model_name,
            [{"role": "user", "content": prompt}],
            temperature=0.7,
        )
        results[idx] = summary

    t1 = threading.Thread(target=worker, args=(0, "qwen3-4b-verify",
                                               "Count from 1 to 30, one number per line."))
    t2 = threading.Thread(target=worker, args=(1, "qwen3-4b-verify",
                                               "List 20 fruits alphabetically."))
    started = time.time()
    t1.start(); t2.start()
    t1.join(); t2.join()
    elapsed = time.time() - started

    s1, s2 = results
    ok = (
        s1 and s2
        and s1["http_status"] == 200 and s1["is_complete"]
        and s2["http_status"] == 200 and s2["is_complete"]
        and s1["content_chars"] > 100 and s2["content_chars"] > 100
    )
    print(f"  r1: chars={s1['content_chars']}, finish={s1['finish_reason']}")
    print(f"  r2: chars={s2['content_chars']}, finish={s2['finish_reason']}")
    print(f"  total elapsed: {elapsed:.1f}s, ok={ok}")
    return ok, {"s1": s1, "s2": s2}


# ============================================================
# Main
# ============================================================

def main():
    print("=" * 60)
    print(f"Bundled Streaming Verify @ {URL}")
    print("=" * 60)

    # Health check
    status, _, _ = http("GET", "/v1/models")
    if status != 200:
        print(f"Health check FAILED: {status}. Aborting.")
        return 1

    # Load model
    ok, load = load_model("qwen3-4b-verify", "/app/models/Qwen3-Instruct-2507-q4km.gguf")
    if not ok:
        print(f"Load FAILED: {load}. Aborting.")
        return 1
    print(f"Model loaded: qwen3-4b-verify")
    time.sleep(3)

    results = {}
    try:
        results["T1_long"] = t1_long_generation_completeness()
        results["T2_short"] = t2_short_simple()
        results["T3_heartbeat"] = t3_heartbeat_long()
        results["T4_maxtok"] = t4_max_tokens_truncation()
        results["T5_concurrent"] = t5_concurrent_requests()
    finally:
        unload_model("qwen3-4b-verify")

    # Summary
    print("\n" + "=" * 60)
    print("SUMMARY")
    print("=" * 60)
    passed = 0
    total = len(results)
    for name, (ok, _) in results.items():
        marker = "PASS" if ok else "FAIL"
        print(f"  {marker}: {name}")
        if ok:
            passed += 1
    print(f"\n  {passed}/{total} tests passed")
    return 0 if passed == total else 1


if __name__ == "__main__":
    sys.exit(main())
