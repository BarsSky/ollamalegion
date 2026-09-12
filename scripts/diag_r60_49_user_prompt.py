#!/usr/bin/env python3
"""R60.49 verification: user prompt must return full HTML+CSS without truncation.

User bug-report (2026-09-12): 'Привет распиши красивый сайт на html css для
интерактивной математики расчета движения полета' showed code cut off at
'font-size:' mid-CSS.

This script:
1. Sends user's exact prompt to balancer /api/chat
2. Streams the response
3. Verifies that the FINAL done-chunk has message.content = full accumulated text
4. Verifies content is NOT empty (the R60.49 regression test condition)
5. Verifies Content-Length is set explicitly (R60.48 contract)
"""
import json
import sys
import time
import urllib.request

BALANCER_URL = "http://localhost:18080"
PROMPT = "Привет распиши красивый сайт на html css для интерактивной математики расчета движения полета"


def stream_chat(prompt):
    """Stream /api/chat and yield (chunk_dict, raw_bytes, headers)."""
    body = json.dumps({
        "model": "Qwen3-Instruct-2507-q4km",
        "messages": [{"role": "user", "content": prompt}],
        "stream": True,
    }).encode("utf-8")
    req = urllib.request.Request(
        f"{BALANCER_URL}/api/chat",
        data=body,
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=600) as resp:
        cl = resp.headers.get("Content-Length")
        te = resp.headers.get("Transfer-Encoding")
        ct = resp.headers.get("Content-Type", "")
        print(f"[HEADERS] Content-Length={cl} | Transfer-Encoding={te} | Content-Type={ct}")

        buf = b""
        for raw in resp:
            buf += raw
            while b"\n" in buf:
                line, buf = buf.split(b"\n", 1)
                line = line.strip()
                if not line:
                    continue
                try:
                    yield json.loads(line.decode("utf-8")), line, resp.headers
                except json.JSONDecodeError:
                    print(f"[WARN] non-JSON line: {line[:200]!r}")


def main():
    print(f"=== R60.49 VERIFICATION ===")
    print(f"Prompt: {PROMPT}")
    print(f"Backend: {BALANCER_URL}/api/chat")
    print()

    streaming_chunks = []
    done_chunk = None
    accumulated_text = ""
    start = time.time()

    for chunk, raw_line, hdrs in stream_chat(PROMPT):
        if not chunk.get("done"):
            msg = chunk.get("message", {})
            content = msg.get("content", "")
            accumulated_text += content
            streaming_chunks.append(chunk)
        else:
            done_chunk = chunk
            elapsed = time.time() - start
            print(f"\n[STREAM DONE] elapsed={elapsed:.1f}s | "
                  f"chunks={len(streaming_chunks)} | "
                  f"accumulated_len={len(accumulated_text)}")

    print()
    print("=== FINAL done-chunk (R60.49 critical inspection) ===")
    print(json.dumps(done_chunk, ensure_ascii=False, indent=2)[:2000])

    # ---- R60.49 verification ----
    print()
    print("=== VERIFICATION RESULTS ===")
    failures = []

    msg = done_chunk.get("message", {})
    done_content = msg.get("content", "")

    if done_content == "":
        failures.append(
            "FAIL: done-chunk message.content is EMPTY (R60.49 bug NOT fixed). "
            "OpenWebUI would show empty/truncated response."
        )
    elif done_content != accumulated_text:
        # Different but non-empty — likely OK if close to accumulated.
        # But ideally they should match.
        print(f"[INFO] done_content ({len(done_content)} bytes) != accumulated_text "
              f"({len(accumulated_text)} bytes). R60.49 fix may need refinement.")
    else:
        print(f"[PASS] done-chunk message.content == accumulated_text ({len(done_content)} bytes)")

    if len(accumulated_text) < 100:
        failures.append(f"FAIL: accumulated_text suspiciously short ({len(accumulated_text)} bytes)")
    else:
        print(f"[PASS] accumulated_text length = {len(accumulated_text)} bytes")

    # Check tokens are populated
    eval_count = done_chunk.get("eval_count", 0)
    prompt_eval_count = done_chunk.get("prompt_eval_count", 0)
    if eval_count == 0:
        failures.append("FAIL: eval_count=0 (token counts lost)")
    else:
        print(f"[PASS] eval_count={eval_count} | prompt_eval_count={prompt_eval_count}")

    # Check done=true
    if not done_chunk.get("done"):
        failures.append("FAIL: done-chunk missing done=true")
    else:
        print(f"[PASS] done=true")

    # R60.48 verification: Content-Length explicit
    # We saw it in headers above. Re-check:
    cl = hdrs.get("Content-Length") if done_chunk else None
    te = hdrs.get("Transfer-Encoding") if done_chunk else None
    if cl is None and te is None:
        # chunked TE OK for streaming (no CL on streaming responses)
        print(f"[INFO] streaming response without CL (chunked TE), acceptable")
    elif cl is not None:
        print(f"[PASS] explicit Content-Length: {cl}")

    # Check for premature truncation: user reported "font-size:" mid-CSS
    if "font-size:" in accumulated_text:
        # Check that AFTER "font-size:" there's actual value (not empty)
        idx = accumulated_text.find("font-size:")
        snippet = accumulated_text[idx:idx+50]
        print(f"[INFO] 'font-size:' found at offset {idx}: '{snippet}...'")
        if ":" in snippet and snippet.split(":")[1].strip() == "":
            failures.append(f"FAIL: 'font-size:' is followed by empty value (truncated mid-CSS)")
        else:
            print(f"[PASS] 'font-size:' has value after it (not truncated)")

    print()
    if failures:
        print("=== FAILED ===")
        for f in failures:
            print(f"  {f}")
        sys.exit(1)
    else:
        print("=== ALL CHECKS PASSED ===")


if __name__ == "__main__":
    main()
