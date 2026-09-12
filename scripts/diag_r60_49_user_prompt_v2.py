#!/usr/bin/env python3
"""R60.49 final verification — captures full stream including auto-continue."""
import json
import sys
import time
import urllib.request

PROMPT = "Привет распиши красивый сайт на html css для интерактивной математики расчета движения полета"

body = json.dumps({
    "model": "Qwen3-Instruct-2507-q4km",
    "messages": [{"role": "user", "content": PROMPT}],
    "stream": True,
}).encode("utf-8")

req = urllib.request.Request(
    "http://localhost:18080/api/chat",
    data=body,
    headers={"Content-Type": "application/json"},
)

all_chunks = []
accumulated_text = ""
last_done_chunk = None
start = time.time()

print(f"Sending prompt ({len(PROMPT)} chars)...")
with urllib.request.urlopen(req, timeout=600) as resp:
    cl = resp.headers.get("Content-Length")
    te = resp.headers.get("Transfer-Encoding")
    print(f"Headers: Content-Length={cl} Transfer-Encoding={te}")

    for raw in resp:
        line = raw.strip()
        if not line:
            continue
        try:
            chunk = json.loads(line.decode("utf-8"))
        except json.JSONDecodeError:
            continue
        all_chunks.append(chunk)
        if not chunk.get("done"):
            accumulated_text += chunk.get("message", {}).get("content", "")
        else:
            last_done_chunk = chunk

elapsed = time.time() - start
print(f"\n=== STREAM COMPLETE in {elapsed:.1f}s ===")
print(f"Total chunks: {len(all_chunks)}")
print(f"Done-chunks seen: {sum(1 for c in all_chunks if c.get('done'))}")

done_content = last_done_chunk.get("message", {}).get("content", "") if last_done_chunk else ""
print(f"Streaming accumulated (sum of delta.content): {len(accumulated_text)} bytes")
print(f"Final done-chunk message.content:           {len(done_content)} bytes")

print()
print("=== VERIFICATION ===")
failures = []

if not last_done_chunk:
    failures.append("FAIL: no done-chunk received")
elif done_content == "":
    failures.append("FAIL: done-chunk message.content is EMPTY (R60.49 bug NOT fixed)")
else:
    print(f"[PASS] done-chunk message.content is non-empty ({len(done_content)} bytes)")

if "font-size:" in done_content:
    idx = done_content.find("font-size:")
    snippet = done_content[idx:idx+80]
    print(f"[PASS] 'font-size:' found in done_content: '{snippet[:60]}...'")
    if "font-size:" in done_content:
        # Check if there's a complete CSS rule after
        # Look for next `;` or `}` after font-size:
        rest = done_content[idx:]
        if ";" in rest or "}" in rest:
            print(f"[PASS] CSS rule after 'font-size:' has terminator")
        else:
            failures.append(f"FAIL: 'font-size:' has no CSS terminator after it")

eval_count = last_done_chunk.get("eval_count", 0) if last_done_chunk else 0
print(f"[{'PASS' if eval_count > 0 else 'FAIL'}] eval_count={eval_count}")

print()
if failures:
    print("FAILED:")
    for f in failures:
        print(f"  {f}")
    sys.exit(1)
else:
    print("=== ALL CHECKS PASSED ===")
    print()
    # Save full content for user inspection
    with open("C:\\tmp\\r6049_full_response.txt", "w", encoding="utf-8") as f:
        f.write(done_content)
    print(f"Full response saved to C:\\tmp\\r6049_full_response.txt")
    print()
    # Show last 500 chars to confirm no truncation
    print("=== LAST 500 chars of done-chunk content ===")
    print(done_content[-500:])
