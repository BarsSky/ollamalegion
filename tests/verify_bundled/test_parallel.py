#!/usr/bin/env python3
"""
test_parallel.py — parallel inference verification for bundled stack.

Tests:
  P1. parallel=1 (default) — baseline long generation
  P2. parallel=2 — long generation with BatchedScheduler
  P3. parallel=2 concurrent requests — both complete fully
  P4. parallel=4 — 4 concurrent requests, none truncated
  P5. parallel=2 + reasoning flag — combined test

Usage: python test_parallel.py [base_url] [token]
"""
import json
import sys
import time
import threading
import urllib.request
import urllib.error
from typing import Optional, Tuple, List

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
    payload = {"model": model, "messages": messages, "stream": True, "max_tokens": 600}
    payload.update(extra)
    started = time.time()
    status, chunks, raw = http("POST", "/v1/chat/completions", payload, stream=True)
    elapsed = time.time() - started

    full_reasoning = ""
    full_content = ""
    saw_reasoning = False
    finish_reason = None
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
    }


# ============================================================
# TESTS
# ============================================================

def p1_baseline():
    """P1: parallel=1 baseline long generation."""
    print("\n[P1] parallel=1 (default) — long generation baseline")
    ok, load = load_model(
        "qwen3-4b-p1",
        "/app/models/Qwen3-Instruct-2507-q4km.gguf",
        parallel=1,
    )
    if not ok:
        return False, {"error": "load failed"}
    time.sleep(3)

    chunks, summary = chat_stream(
        "qwen3-4b-p1",
        [{"role": "user", "content": "Write a 300-word essay about machine learning."}],
        temperature=0.7,
    )
    unload_model("qwen3-4b-p1")

    ok = (
        summary["http_status"] == 200
        and summary["is_complete"]
        and summary["content_chars"] > 1000
    )
    print(f"  chunks={summary['chunks_count']}, chars={summary['content_chars']}, "
          f"finish={summary['finish_reason']}, ok={ok}")
    return ok, summary


def p2_parallel2_long():
    """P2: parallel=2 — long generation, no token drops."""
    print("\n[P2] parallel=2 — long generation (BatchedScheduler)")
    ok, load = load_model(
        "qwen3-4b-p2",
        "/app/models/Qwen3-Instruct-2507-q4km.gguf",
        parallel=2,
    )
    if not ok:
        return False, {"error": "load failed"}
    time.sleep(3)

    chunks, summary = chat_stream(
        "qwen3-4b-p2",
        [{"role": "user", "content": "Write a 500-word essay about deep learning architectures. Cover CNN, RNN, and Transformer. Use subheadings."}],
        temperature=0.7,
    )
    unload_model("qwen3-4b-p2")

    # After Round 17.1 fix (buffer 128→4096), no token drops expected
    ok = (
        summary["http_status"] == 200
        and summary["is_complete"]
        and summary["content_chars"] > 1500
        and summary["chunks_count"] > 200
    )
    print(f"  chunks={summary['chunks_count']}, chars={summary['content_chars']}, "
          f"raw_bytes={summary['raw_bytes']}, finish={summary['finish_reason']}, ok={ok}")
    return ok, summary


def p3_parallel2_concurrent():
    """P3: parallel=2 + 2 concurrent requests — both complete."""
    print("\n[P3] parallel=2 + 2 concurrent requests")
    ok, load = load_model(
        "qwen3-4b-p3",
        "/app/models/Qwen3-Instruct-2507-q4km.gguf",
        parallel=2,
    )
    if not ok:
        return False, {"error": "load failed"}
    time.sleep(3)

    results = [None, None]

    def worker(idx, prompt):
        _, summary = chat_stream(
            "qwen3-4b-p3",
            [{"role": "user", "content": prompt}],
            temperature=0.7,
        )
        results[idx] = summary

    t1 = threading.Thread(target=worker, args=(0, "List 25 capital cities with their countries, numbered."))
    t2 = threading.Thread(target=worker, args=(1, "Write a 200-word description of the solar system."))
    started = time.time()
    t1.start(); t2.start()
    t1.join(); t2.join()
    elapsed = time.time() - started

    unload_model("qwen3-4b-p3")

    s1, s2 = results
    ok = (
        s1 and s2
        and s1["http_status"] == 200 and s1["is_complete"]
        and s2["http_status"] == 200 and s2["is_complete"]
        and s1["content_chars"] > 500 and s2["content_chars"] > 500
    )
    print(f"  r1: chars={s1['content_chars']}, finish={s1['finish_reason']}")
    print(f"  r2: chars={s2['content_chars']}, finish={s2['finish_reason']}")
    print(f"  total elapsed: {elapsed:.1f}s, ok={ok}")
    return ok, {"s1": s1, "s2": s2}


def p4_parallel4_concurrent():
    """P4: parallel=4 + 4 concurrent requests — stress test."""
    print("\n[P4] parallel=4 + 4 concurrent requests (stress)")
    ok, load = load_model(
        "qwen3-4b-p4",
        "/app/models/Qwen3-Instruct-2507-q4km.gguf",
        parallel=4,
    )
    if not ok:
        return False, {"error": "load failed"}
    time.sleep(3)

    results = [None] * 4

    def worker(idx, prompt):
        _, summary = chat_stream(
            "qwen3-4b-p4",
            [{"role": "user", "content": prompt}],
            temperature=0.7,
            max_tokens=200,
        )
        results[idx] = summary

    prompts = [
        "Count from 1 to 30.",
        "List 15 colors.",
        "Name 10 animals.",
        "Recite the alphabet.",
    ]
    threads = [threading.Thread(target=worker, args=(i, p)) for i, p in enumerate(prompts)]
    started = time.time()
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    elapsed = time.time() - started

    unload_model("qwen3-4b-p4")

    all_complete = all(r and r["http_status"] == 200 and r["is_complete"] for r in results)
    all_have_content = all(r and r["content_chars"] > 30 for r in results)
    ok = all_complete and all_have_content
    print(f"  All complete: {all_complete}, all have content: {all_have_content}")
    for i, r in enumerate(results):
        print(f"  r{i+1}: chars={r['content_chars']}, finish={r['finish_reason']}")
    print(f"  total elapsed: {elapsed:.1f}s, ok={ok}")
    return ok, results


def p5_parallel2_with_reasoning():
    """P5: parallel=2 + enableReasoning — combined test."""
    print("\n[P5] parallel=2 + enableReasoning (combined)")
    ok, load = load_model(
        "qwen3-4b-p5",
        "/app/models/Qwen3-Instruct-2507-q4km.gguf",
        parallel=2,
        enableReasoning=True,
    )
    if not ok:
        return False, {"error": "load failed"}
    time.sleep(3)

    chunks, summary = chat_stream(
        "qwen3-4b-p5",
        [{"role": "user", "content": "Compute 47 * 53. Show ALL steps in <think> tags, then give the answer."}],
        temperature=0.3,
    )
    unload_model("qwen3-4b-p5")

    ok = (
        summary["http_status"] == 200
        and summary["is_complete"]
        and summary["saw_reasoning_field"]
        and summary["reasoning_chars"] > 30
    )
    print(f"  reasoning: {summary['reasoning_chars']}c, content: {summary['content_chars']}c, "
          f"complete={summary['is_complete']}, ok={ok}")
    return ok, summary


# ============================================================
# Main
# ============================================================

def main():
    print("=" * 60)
    print(f"Bundled Parallel Verify @ {URL}")
    print("=" * 60)

    status, _, _ = http("GET", "/v1/models")
    if status != 200:
        print(f"Health check FAILED: {status}")
        return 1

    results = {}
    try:
        results["P1_baseline"] = p1_baseline()
        results["P2_parallel2"] = p2_parallel2_long()
        results["P3_concurrent"] = p3_parallel2_concurrent()
        results["P4_stress"] = p4_parallel4_concurrent()
        results["P5_parallel_reasoning"] = p5_parallel2_with_reasoning()
    finally:
        for name in ["qwen3-4b-p1", "qwen3-4b-p2", "qwen3-4b-p3",
                     "qwen3-4b-p4", "qwen3-4b-p5"]:
            try:
                unload_model(name)
            except Exception:
                pass

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
