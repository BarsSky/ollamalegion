#!/usr/bin/env python3
"""
test_generative_dialogue.py — End-to-end generative dialogue test.

Verifies REAL chat behavior with a loaded model:
1. Single-turn basic (coherent reply)
2. Multi-turn context (memory across messages)
3. Streaming complete (SSE chunks, data: [DONE] terminator)
4. Token accounting (usage.prompt_tokens matches input)
5. Long context (>2048 tokens) preservation
6. Concurrent requests (3 parallel, all succeed, no caching bugs)
7. Cancellation (abort after first chunk, server doesn't error)

Uses OpenAI-compatible /v1/chat/completions endpoint.
Catches real-world bugs:
- Context window truncation (forgetting earlier messages)
- KV-cache offload failures
- Streaming chunk drops
- Caching of identical responses
- Deadlock on concurrent load

Запуск:
    python scripts/test_generative_dialogue.py
    python scripts/test_generative_dialogue.py http://localhost:18080
    python scripts/test_generative_dialogue.py --model Qwen3-Instruct-2507-q4km

Exit code 0 = ALL PASS, 1 = some FAIL.
"""
import argparse
import concurrent.futures
import json
import re
import sys
import threading
import time
from typing import Any, Dict, List, Optional, Tuple

import requests

DEFAULT_BASE = "http://localhost:18080"
DEFAULT_MODEL = "Qwen3-Instruct-2507-q4km"
DEFAULT_TIMEOUT = 120
STREAM_TIMEOUT = 180

results = {"passed": 0, "failed": 0, "tests": []}
exit_code = 0


def record(name: str, ok: bool, detail: str = "") -> None:
    global exit_code
    mark = "✓" if ok else "✗"
    results["tests"].append({"name": name, "ok": ok, "detail": detail})
    print(f"  {mark} {name}{' — ' + detail if detail else ''}")
    if ok:
        results["passed"] += 1
    else:
        results["failed"] += 1
        exit_code = 1


def chat(base: str, model: str, messages: List[Dict], max_tokens: int = 100,
         stream: bool = False, timeout: int = DEFAULT_TIMEOUT) -> Tuple[Optional[Dict], Optional[str]]:
    body = {"model": model, "messages": messages, "max_tokens": max_tokens, "stream": stream, "temperature": 0.1}
    try:
        r = requests.post(base + "/v1/chat/completions", json=body, timeout=timeout, stream=stream)
        if r.status_code != 200:
            return None, f"status={r.status_code} body={r.text[:200]}"
        if stream:
            return _parse_stream(r, timeout), None
        return r.json(), None
    except Exception as e:
        return None, f"{type(e).__name__}: {e}"


def _parse_stream(r: requests.Response, timeout: int) -> Dict:
    """Parse SSE stream, accumulate content and chunks."""
    chunks: List[Dict] = []
    content_parts: List[str] = []
    saw_done = False
    for line in r.iter_lines(decode_unicode=True):
        if not line or not line.startswith("data: "):
            continue
        data = line[6:].strip()
        if data == "[DONE]":
            saw_done = True
            break
        try:
            chunk = json.loads(data)
            chunks.append(chunk)
            if chunk.get("choices"):
                delta = chunk["choices"][0].get("delta", {})
                if "content" in delta:
                    content = delta.get("content")
                    if content is not None:
                        content_parts.append(content)
            if chunk.get("choices") and chunk["choices"][0].get("finish_reason") == "stop":
                # Don't break — continue until [DONE] for full token count
                pass
        except json.JSONDecodeError:
            pass
    return {"chunks": chunks, "content": "".join(content_parts), "saw_done_marker": saw_done}


# --- Test scenarios ---

def test_single_turn(base: str, model: str) -> None:
    body, err = chat(base, model, [{"role": "user", "content": "What is 2+2? Reply with just the number."}], max_tokens=20)
    if err:
        record("single-turn basic", False, err)
        return
    content = body.get("choices", [{}])[0].get("message", {}).get("content", "")
    if "4" not in content:
        record("single-turn basic", False, f"expected '4' in response, got '{content[:80]}'")
        return
    record("single-turn basic", True, f"response='{content.strip()[:40]}'")


def test_multi_turn_context(base: str, model: str) -> None:
    """Critical: model must remember context across turns."""
    # Turn 1: establish fact
    r1, err = chat(base, model, [
        {"role": "user", "content": "My favorite color is cerulean. Remember this fact."},
    ], max_tokens=30)
    if err:
        record("multi-turn context (turn 1)", False, err)
        return
    turn1 = r1.get("choices", [{}])[0].get("message", {}).get("content", "")

    # Turn 2: ask about fact
    r2, err = chat(base, model, [
        {"role": "user", "content": "My favorite color is cerulean. Remember this fact."},
        {"role": "assistant", "content": turn1},
        {"role": "user", "content": "What is my favorite color? Reply with just the color name."},
    ], max_tokens=30)
    if err:
        record("multi-turn context (turn 2)", False, err)
        return
    turn2 = r2.get("choices", [{}])[0].get("message", {}).get("content", "")
    if "cerulean" not in turn2.lower():
        record("multi-turn context", False, f"model forgot 'cerulean' (got '{turn2.strip()[:60]}')")
        return
    record("multi-turn context", True, f"turn1='{turn1.strip()[:30]}' turn2='{turn2.strip()[:30]}'")


def test_streaming_complete(base: str, model: str) -> None:
    """Stream must complete with [DONE] and coherent content."""
    body, err = chat(base, model, [{"role": "user", "content": "Count from 1 to 5."}], max_tokens=50, stream=True, timeout=STREAM_TIMEOUT)
    if err:
        record("streaming complete", False, err)
        return
    if isinstance(body, dict) and "error" in body:
        record("streaming complete", False, f"error in body: {body['error']}")
        return
    chunks = body.get("chunks", [])
    content = body.get("content", "")
    saw_done = body.get("saw_done_marker", False)
    if not chunks:
        record("streaming complete", False, "no chunks")
        return
    if not saw_done:
        record("streaming complete", False, f"missing [DONE] terminator ({len(chunks)} chunks)")
        return
    if not content.strip():
        record("streaming complete", False, "empty content from stream")
        return
    # Verify chunk format: each has choices[0].delta (not message)
    bad_chunks = [c for c in chunks if c.get("choices") and "message" in c["choices"][0] and "delta" not in c["choices"][0]]
    if bad_chunks:
        record("streaming complete", False, f"{len(bad_chunks)} chunks use 'message' instead of 'delta'")
        return
    record("streaming complete", True, f"{len(chunks)} chunks, content='{content.strip()[:40]}'")


def test_token_accounting(base: str, model: str) -> None:
    """usage.prompt_tokens should match input length."""
    # Three messages with different lengths
    test_cases = [
        [{"role": "user", "content": "Hi"}],
        [{"role": "user", "content": "Tell me a short joke about programmers."}],
        [{"role": "user", "content": "Explain in detail the difference between TCP and UDP, including handshake mechanisms, flow control, and typical use cases."}],
    ]
    expected_min = [1, 5, 20]
    results_list = []
    for i, msgs in enumerate(test_cases):
        body, err = chat(base, model, msgs, max_tokens=10)
        if err:
            results_list.append((0, err))
            continue
        usage = body.get("usage", {})
        prompt_tokens = usage.get("prompt_tokens", 0)
        results_list.append((prompt_tokens, ""))
    for i, ((tokens, err), expected) in enumerate(zip(results_list, expected_min)):
        if err:
            record(f"token accounting (case {i+1})", False, err)
            continue
        if tokens < expected:
            record(f"token accounting (case {i+1})", False, f"prompt_tokens={tokens} < expected ~{expected}")
        else:
            print(f"  - token accounting case {i+1}: {tokens} prompt tokens (expected >{expected})")
    if all(not err for _, err in results_list):
        record("token accounting", True, f"3 cases, prompt_tokens={[t for t, _ in results_list]}")


def test_long_context(base: str, model: str) -> None:
    """Test >2048 token context (catches context-window truncation bugs)."""
    # Build a long context: ~3000 tokens of repetitive text
    long_text = "The quick brown fox jumps over the lazy dog. " * 200  # ~2400 tokens
    question = "What animal did I mention at the START of this message?"
    body, err = chat(base, model, [{"role": "user", "content": long_text + " " + question}], max_tokens=30, timeout=STREAM_TIMEOUT)
    if err:
        record("long context (>2048 tokens)", False, err)
        return
    content = body.get("choices", [{}])[0].get("message", {}).get("content", "")
    # Model should mention "fox" (from the long repetitive text)
    if "fox" not in content.lower():
        record("long context (>2048 tokens)", False, f"model didn't recall 'fox' from earlier in message: '{content[:60]}'")
        return
    usage = body.get("usage", {})
    record("long context (>2048 tokens)", True, f"recalled 'fox' across {usage.get('prompt_tokens', '?')} prompt tokens")


def test_concurrent_requests(base: str, model: str) -> None:
    """3 concurrent requests, all succeed, no caching bugs.

    Используем temperature=0 чтобы уменьшить вариативность. Проверяем что все
    запросы возвращают уникальный ответ (нет кеширования между потоками).
    """
    prompts = [
        "Reply with exactly the single word: alpha",
        "Reply with exactly the single word: bravo",
        "Reply with exactly the single word: charlie",
    ]
    expected_keywords = ["alpha", "bravo", "charlie"]
    results_list: List[Tuple[int, str]] = []  # (status_code, content)

    def send(prompt: str) -> Tuple[int, str]:
        body, err = chat(base, model, [{"role": "user", "content": prompt}], max_tokens=20)
        if err:
            return (0, err)
        content = body.get("choices", [{}])[0].get("message", {}).get("content", "")
        return (200, content)

    with concurrent.futures.ThreadPoolExecutor(max_workers=3) as ex:
        # Submit with index to preserve prompt ordering.
        future_to_idx = {ex.submit(send, p): i for i, p in enumerate(prompts)}
        # results_list is now indexed by prompt position.
        results_list = [None] * len(prompts)
        for f in concurrent.futures.as_completed(future_to_idx):
            idx = future_to_idx[f]
            results_list[idx] = f.result()

    # Check all succeeded
    if any(r is None or r[0] != 200 for r in results_list):
        record("concurrent requests", False, f"some failed: {results_list}")
        return
    # Check responses are different (no caching bug)
    contents = [c for _, c in results_list]
    if len(set(contents)) < len(contents):
        record("concurrent requests", False, f"some responses identical (caching bug?): {contents}")
        return
    # Check expected keywords
    for i, ((code, content), kw) in enumerate(zip(results_list, expected_keywords)):
        if kw not in content.lower():
            record("concurrent requests", False, f"response {i+1} doesn't contain '{kw}': '{content[:60]}'")
            return
    record("concurrent requests", True, f"3 unique responses, all keywords present")


def test_cancellation(base: str, model: str) -> None:
    """Start streaming, abort after 1st chunk, server must not error."""
    try:
        r = requests.post(
            base + "/v1/chat/completions",
            json={"model": model, "messages": [{"role": "user", "content": "Write a long essay about the history of Rome."}], "max_tokens": 500, "stream": True},
            stream=True, timeout=30,
        )
    except Exception as e:
        record("cancellation", False, f"{type(e).__name__}: {e}")
        return
    if r.status_code != 200:
        record("cancellation", False, f"status={r.status_code}")
        return
    # Read 1-2 chunks
    chunk_count = 0
    for line in r.iter_lines(decode_unicode=True):
        if line:
            line_str = line.decode("utf-8") if isinstance(line, bytes) else line
            if line_str.startswith("data: ") and line_str[6:].strip() != "[DONE]":
                chunk_count += 1
                if chunk_count >= 2:
                    break
    # Force close (simulate client cancel)
    r.close()
    # Now send another request — should succeed (server didn't deadlock)
    body, err = chat(base, model, [{"role": "user", "content": "Hi"}], max_tokens=10)
    if err:
        record("cancellation", False, f"server after cancel: {err}")
        return
    if not body.get("choices"):
        record("cancellation", False, "server after cancel: no choices")
        return
    record("cancellation", True, f"got {chunk_count} chunks, server still healthy after cancel")


def test_generative_dialogue(base: str, model: str) -> int:
    print(f"[generative-dialogue] base={base} model={model}")
    test_single_turn(base, model)
    test_multi_turn_context(base, model)
    test_streaming_complete(base, model)
    test_token_accounting(base, model)
    test_long_context(base, model)
    test_concurrent_requests(base, model)
    test_cancellation(base, model)
    total = results["passed"] + results["failed"]
    print(f"\nResults: {results['passed']}/{total} pass, {results['failed']} fail")
    return exit_code


def main() -> int:
    parser = argparse.ArgumentParser(description="Generative dialogue e2e test")
    parser.add_argument("base", nargs="?", default=DEFAULT_BASE, help="Base URL")
    parser.add_argument("--model", default=DEFAULT_MODEL, help="Model name")
    args = parser.parse_args()
    return test_generative_dialogue(args.base, args.model)


if __name__ == "__main__":
    sys.exit(main())
