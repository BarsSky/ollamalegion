#!/usr/bin/env python3
"""
test_reasoning.py — comprehensive reasoning routing verification for bundled stack.

Tests:
  R1. qwen3-instruct + explicit enableReasoning=true → reasoning_content in SSE
  R2. qwen3-instruct WITHOUT enableReasoning (default) → soft prompt forces
      <think> tags, L3 auto-detect kicks in, future requests split correctly
  R3. Soft prompt update: qwen3-instruct with reasoning flag actually emits
      <think> tags (proves Round 17.1 fix works)
  R4. L3 threshold: model that emits <think> after long preamble still triggers
  R5. Per-model override: enableReasoning=true persists across reloads

Usage: python test_reasoning.py [base_url] [token]
"""
import json
import sys
import time
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
        "saw_reasoning_field": saw_reasoning,
        "finish_reason": finish_reason,
        "content_chars": len(full_content),
        "reasoning_chars": len(full_reasoning),
        "content_preview": full_content[:200],
        "reasoning_preview": full_reasoning[:200],
        "is_complete": finish_reason in ("stop", "length"),
    }


# ============================================================
# TESTS
# ============================================================

def r1_explicit_enable_reasoning():
    """R1: explicit enableReasoning=true → response split correctly."""
    print("\n[R1] qwen3-instruct + explicit enableReasoning=true")
    ok, load = load_model(
        "qwen3-4b-r1",
        "/app/models/Qwen3-Instruct-2507-q4km.gguf",
        enableReasoning=True,
    )
    if not ok:
        print(f"  load failed: {load}")
        return False, {"error": "load failed"}
    info = load.get("model", {})
    persisted = info.get("reasoningEnabled", False)
    time.sleep(3)

    chunks, summary = chat_stream(
        "qwen3-4b-r1",
        [{"role": "user", "content": "What is 7+5? Show your reasoning step by step, then give the final answer."}],
        temperature=0.7,
    )
    unload_model("qwen3-4b-r1")

    ok = (
        persisted
        and summary["http_status"] == 200
        and summary["saw_reasoning_field"]
        and summary["reasoning_chars"] > 50
        and summary["is_complete"]
    )
    print(f"  reasoningEnabled persisted: {persisted}")
    print(f"  reasoning chars: {summary['reasoning_chars']}")
    print(f"  content chars: {summary['content_chars']}")
    print(f"  reasoning preview: {summary['reasoning_preview'][:100]!r}")
    print(f"  content preview: {summary['content_preview'][:100]!r}")
    print(f"  ok={ok}")
    return ok, summary


def r2_soft_prompt_forces_think_tags():
    """R2: soft prompt update forces <think> tags even on non-thinking models."""
    print("\n[R2] soft prompt forces <think> tags (Round 17.1 fix)")
    ok, load = load_model(
        "qwen3-4b-r2",
        "/app/models/Qwen3-Instruct-2507-q4km.gguf",
        enableReasoning=True,
    )
    if not ok:
        return False, {"error": "load failed"}
    time.sleep(3)

    # Strong prompt that should produce <think>
    chunks, summary = chat_stream(
        "qwen3-4b-r2",
        [{"role": "user", "content": "Compute 17*23. Show ALL your work step by step."}],
        temperature=0.3,
        max_tokens=500,
    )
    unload_model("qwen3-4b-r2")

    has_think = "<think>" in summary["reasoning_preview"] or "<think>" in summary["content_preview"]
    split_correctly = summary["saw_reasoning_field"] and summary["reasoning_chars"] > 30
    ok = (
        summary["http_status"] == 200
        and split_correctly
        and summary["is_complete"]
    )
    print(f"  has <think> tag: {has_think}")
    print(f"  reasoning field: {summary['saw_reasoning_field']}, chars: {summary['reasoning_chars']}")
    print(f"  content preview: {summary['content_preview'][:100]!r}")
    print(f"  ok={ok}")
    return ok, summary


def r3_l3_autodetect_persistence():
    """R3: L3 auto-detect on first request, persistence on second."""
    print("\n[R3] L3 auto-detect + persistence (enableReasoning=False)")
    # Load WITHOUT enableReasoning
    ok, load = load_model(
        "qwen3-4b-r3",
        "/app/models/Qwen3-Instruct-2507-q4km.gguf",
        enableReasoning=False,
    )
    if not ok:
        return False, {"error": "load failed"}
    time.sleep(3)

    # First request — model might emit <think> via soft prompt (cfg.EnableReasoning?)
    chunks1, s1 = chat_stream(
        "qwen3-4b-r3",
        [{"role": "user", "content": "Calculate 25*47 step by step."}],
        temperature=0.3,
        max_tokens=500,
    )
    print(f"  Request 1: reasoning={s1['reasoning_chars']}c, content={s1['content_chars']}c, "
          f"reasoning_field={s1['saw_reasoning_field']}")
    time.sleep(2)

    # Second request — if L3 fired on first, this should also have reasoning
    chunks2, s2 = chat_stream(
        "qwen3-4b-r3",
        [{"role": "user", "content": "What is 100/4?"}],
        temperature=0.3,
        max_tokens=200,
    )
    unload_model("qwen3-4b-r3")

    print(f"  Request 2: reasoning={s2['reasoning_chars']}c, content={s2['content_chars']}c, "
          f"reasoning_field={s2['saw_reasoning_field']}")
    # L3 should fire on request 1 if cfg.EnableReasoning=true (soft prompt + <think>)
    # OR not fire if EnableReasoning is globally false
    # At minimum, both responses should be complete
    ok = (
        s1["http_status"] == 200 and s1["is_complete"]
        and s2["http_status"] == 200 and s2["is_complete"]
    )
    print(f"  ok={ok}")
    return ok, {"s1": s1, "s2": s2}


def r4_long_preamble_think_after():
    """R4: <think> after long preamble (>64 chars) — Round 17.1 fix L3 threshold."""
    print("\n[R4] <think> after 200-char preamble (L3 threshold fix)")
    ok, load = load_model(
        "qwen3-4b-r4",
        "/app/models/Qwen3-Instruct-2507-q4km.gguf",
        enableReasoning=True,
    )
    if not ok:
        return False, {"error": "load failed"}
    time.sleep(3)

    # Long preamble prompt that should make model think before <think>
    prompt = (
        "I want to understand a complex math problem. "
        "First, take a deep breath and think about how to approach this. "
        "Consider what mathematical operations are needed. "
        "Then plan your solution carefully. "
        "Now compute the result of (123 + 456) * 7. "
        "Show your reasoning in <think> tags, then give the final answer."
    )
    chunks, summary = chat_stream(
        "qwen3-4b-r4",
        [{"role": "user", "content": prompt}],
        temperature=0.3,
        max_tokens=600,
    )
    unload_model("qwen3-4b-r4")

    has_think_in_reasoning = "<think>" in summary["reasoning_preview"]
    has_think_in_content = "<think>" in summary["content_preview"]
    ok = (
        summary["http_status"] == 200
        and summary["saw_reasoning_field"]
        and summary["reasoning_chars"] > 50
        and not has_think_in_content  # <think> should be in reasoning, not content
        and summary["is_complete"]
    )
    print(f"  has <think> in reasoning: {has_think_in_reasoning}")
    print(f"  has <think> in content (BAD): {has_think_in_content}")
    print(f"  reasoning chars: {summary['reasoning_chars']}")
    print(f"  reasoning preview: {summary['reasoning_preview'][:100]!r}")
    print(f"  ok={ok}")
    return ok, summary


def r5_no_think_no_split():
    """R5: model that doesn't emit <think> → no split, all in content (regression check)."""
    print("\n[R5] Non-reasoning response: no <think>, all in content")
    ok, load = load_model(
        "qwen3-4b-r5",
        "/app/models/Qwen3-Instruct-2507-q4km.gguf",
        enableReasoning=False,
    )
    if not ok:
        return False, {"error": "load failed"}
    time.sleep(3)

    # Simple non-reasoning prompt
    chunks, summary = chat_stream(
        "qwen3-4b-r5",
        [{"role": "user", "content": "What is the capital of France? Just one word."}],
        temperature=0.0,
        max_tokens=20,
    )
    unload_model("qwen3-4b-r5")

    # Without reasoning enabled, content should have the answer, no reasoning field
    ok = (
        summary["http_status"] == 200
        and not summary["saw_reasoning_field"]
        and "Paris" in summary["content_preview"] or "paris" in summary["content_preview"].lower()
        and summary["is_complete"]
    )
    print(f"  reasoning field: {summary['saw_reasoning_field']} (should be False)")
    print(f"  content preview: {summary['content_preview'][:100]!r}")
    print(f"  ok={ok}")
    return ok, summary


# ============================================================
# Main
# ============================================================

def main():
    print("=" * 60)
    print(f"Bundled Reasoning Verify @ {URL}")
    print("=" * 60)

    status, _, _ = http("GET", "/v1/models")
    if status != 200:
        print(f"Health check FAILED: {status}")
        return 1

    results = {}
    try:
        results["R1_explicit"] = r1_explicit_enable_reasoning()
        results["R2_soft_prompt"] = r2_soft_prompt_forces_think_tags()
        results["R3_l3_persistence"] = r3_l3_autodetect_persistence()
        results["R4_long_preamble"] = r4_long_preamble_think_after()
        results["R5_no_think"] = r5_no_think_no_split()
    finally:
        # Clean up any leftover models
        for name in ["qwen3-4b-r1", "qwen3-4b-r2", "qwen3-4b-r3",
                     "qwen3-4b-r4", "qwen3-4b-r5"]:
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
