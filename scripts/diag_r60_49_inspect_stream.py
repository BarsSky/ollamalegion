#!/usr/bin/env python3
"""Inspect the raw NDJSON stream to understand content vs accumulated mismatch."""
import json
import urllib.request

PROMPT = "Привет, кратко опиши что такое Python"

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

with urllib.request.urlopen(req, timeout=120) as resp:
    chunks = []
    for raw in resp:
        line = raw.strip()
        if not line:
            continue
        try:
            chunks.append(json.loads(line.decode("utf-8")))
        except json.JSONDecodeError:
            print(f"NON-JSON: {line!r}")

print(f"Total chunks: {len(chunks)}")
print()
# Show first 5 chunks (truncated)
print("=== First 5 chunks ===")
for i, c in enumerate(chunks[:5]):
    msg = c.get("message", {})
    print(f"  [{i}] done={c.get('done')} content={msg.get('content','')!r:.100} "
          f"reasoning={msg.get('reasoning_content','')!r:.50}")

# Show last 5 chunks (truncated)
print()
print("=== Last 5 chunks ===")
for i, c in enumerate(chunks[-5:]):
    idx = len(chunks) - 5 + i
    msg = c.get("message", {})
    print(f"  [{idx}] done={c.get('done')} content={msg.get('content','')!r:.100} "
          f"reasoning={msg.get('reasoning_content','')!r:.50} "
          f"eval_count={c.get('eval_count')}")

# Compute accumulated
acc = ""
for c in chunks:
    if not c.get("done"):
        acc += c.get("message", {}).get("content", "")

last = chunks[-1]
done_content = last.get("message", {}).get("content", "")
print()
print(f"accumulated_text (sum of non-done.content) = {len(acc)} bytes")
print(f"done-chunk message.content                 = {len(done_content)} bytes")
print(f"diff (done - accumulated)                  = {len(done_content) - len(acc)} bytes")
print()
print("=== Last chunk content (full) ===")
print(done_content[:2000])
