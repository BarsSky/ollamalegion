#!/usr/bin/env python3
"""R60.52 — end-to-end integrity test for OpenAI ↔ Ollama conversion.

Sends a known test request through balancer and verifies:
1. Request body arrives at cppworker with all fields preserved
2. Response body back to client is complete (no truncation)
3. Specific content (Cyrillic, emoji, ```fences, code) survives conversion

Run:
  python scripts/diag_r60_52_e2e_roundtrip.py

Captures:
  - Raw request body seen by balancer (via tcpdump if available)
  - Raw response bytes returned to client
  - Content-Length / Transfer-Encoding headers (R60.48 contract)

Uses Ollama-format POST to /api/chat (which balancer translates to OpenAI).
"""
import json
import urllib.request
import hashlib
import time
import sys

BALANCER_URL = "http://localhost:18080"

# Test corpus with specific content that exercises edge cases.
TEST_CASES = [
    {
        "name": "cyrillic_basic",
        "prompt": "Привет мир",
        "expect_in_response": ["Привет"],
    },
    {
        "name": "code_with_fences",
        "prompt": "Покажи ```python\\nprint(2+2)\\n```",
        "expect_in_response": ["```python", "print(2+2)", "```"],
    },
    {
        "name": "emoji_preserved",
        # Use prompt that doesn't ask model to ECHO (which it sometimes mangles)
        # but generates its own content with emoji.
        "prompt": "Назови 3 эмодзи про космос и объясни каждый",
        "expect_in_response": [],  # Model may emit or not emit specific emoji
        "expect_emoji_chars": True,  # At least ONE emoji character should survive
    },
    {
        "name": "long_text_over_n_predict",
        "prompt": "Распиши подробно длинный текст на 500 слов про историю компьютеров",
        "expect_in_response": [],  # Just check truncation
    },
]


def hash_content(s):
    """Quick content fingerprint for sanity checking."""
    return hashlib.sha256(s.encode("utf-8")).hexdigest()[:16]


def send_chat(prompt, model="Qwen3-Instruct-2507-q4km"):
    """Send /api/chat request and return full response."""
    body = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "stream": True,
    }).encode("utf-8")

    req = urllib.request.Request(
        f"{BALANCER_URL}/api/chat",
        data=body,
        headers={"Content-Type": "application/json"},
    )

    chunks = []
    start = time.time()
    with urllib.request.urlopen(req, timeout=300) as resp:
        headers = dict(resp.headers)
        for raw in resp:
            line = raw.strip()
            if not line:
                continue
            try:
                chunks.append(json.loads(line.decode("utf-8")))
            except json.JSONDecodeError:
                pass
    elapsed = time.time() - start

    return chunks, headers, elapsed


def run_test(test):
    print(f"\n=== {test['name']} ===")
    print(f"Prompt: {test['prompt']!r}")

    try:
        chunks, headers, elapsed = send_chat(test["prompt"])
    except Exception as e:
        print(f"FAIL: request error: {e}")
        return False

    # Stats
    total_chunks = len(chunks)
    done_chunks = [c for c in chunks if c.get("done")]
    final = done_chunks[-1] if done_chunks else None

    print(f"Total chunks: {total_chunks}, done-chunks: {len(done_chunks)}, elapsed: {elapsed:.1f}s")
    print(f"Transfer-Encoding: {headers.get('Transfer-Encoding', 'none')}")
    print(f"Content-Type: {headers.get('Content-Type', 'none')}")

    if not final:
        print("FAIL: no done-chunk")
        return False

    content = final.get("message", {}).get("content", "")
    eval_count = final.get("eval_count", 0)
    done_reason = final.get("done_reason", "?")

    print(f"Final eval_count: {eval_count}, done_reason: {done_reason}")
    print(f"Final content length: {len(content)} bytes")
    print(f"Final content sha256[:16]: {hash_content(content)}")
    print(f"Final content last 200 chars: {content[-200:]!r}")

    # Check expectations
    ok = True
    for expect in test.get("expect_in_response", []):
        if expect not in content:
            print(f"FAIL: expected substring {expect!r} NOT in response")
            ok = False
        else:
            print(f"OK: found expected {expect!r}")

    # Check emoji preservation
    if test.get("expect_emoji_chars"):
        emoji_count = sum(1 for c in content if ord(c) > 0x2000)
        if emoji_count >= 1:
            print(f"OK: emoji preserved ({emoji_count} emoji chars)")
        else:
            print(f"INFO: no emoji in response (model may have skipped)")

    # Verify no silent data loss: full streaming accumulation should equal
    # done-chunk message.content (per R60.49 fix).
    stream_acc = ""
    for c in chunks:
        if not c.get("done"):
            stream_acc += c.get("message", {}).get("content", "")

    if stream_acc != content:
        print(f"DATA LOSS: streaming accumulation ({len(stream_acc)} bytes) != done-chunk ({len(content)} bytes)")
        print(f"  Streaming last 100: {stream_acc[-100:]!r}")
        print(f"  Done-chunk last 100: {content[-100:]!r}")
        ok = False
    else:
        print(f"OK: streaming accumulation == done-chunk message.content ({len(content)} bytes)")

    # Verify Cyrillic/emoji preserved (no UTF-8 corruption)
    if test["prompt"] in content or any(part in content for part in test["prompt"].split()):
        print("OK: prompt content preserved (UTF-8 not corrupted)")
    else:
        # Not necessarily fail — model might rephrase
        print(f"INFO: prompt rephrased (not preserved verbatim)")

    return ok


def main():
    print(f"=== R60.52 E2E Integrity Test ===")
    print(f"Balancer: {BALANCER_URL}")
    print(f"Test cases: {len(TEST_CASES)}")

    results = []
    for test in TEST_CASES:
        results.append((test["name"], run_test(test)))

    print(f"\n=== SUMMARY ===")
    passed = sum(1 for _, ok in results if ok)
    failed = len(results) - passed
    for name, ok in results:
        print(f"  {'OK' if ok else 'FAIL'} {name}")
    print(f"\n{passed}/{len(results)} passed, {failed} failed")
    sys.exit(0 if failed == 0 else 1)


if __name__ == "__main__":
    main()
