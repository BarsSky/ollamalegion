#!/usr/bin/env python3
"""R60.19 — Reproduce OpenWebUI "lost closing backticks" issue.

User reports: when response contains a code block, the closing ```
is lost. Last line of code shows garbled "♦♦♦♦" instead of proper
text. Issue happens on repeated requests with code blocks.

This script:
1. Sends 2-message conversation (assistant code history + new request)
2. Captures full response byte-by-byte
3. Checks if response has matching closing ```
4. Detects garbled UTF-8 sequences at end of response
"""
import json
import time
import requests
from datetime import datetime

BALANCER = "http://localhost:18080"
API_TOKEN = "changeme-bundled-with-agent-token"
MODEL = "Qwen3-Instruct-2507-q4km"

# 2-message history simulating repeated request with code block
body = {
    "model": MODEL,
    "messages": [
        {"role": "user", "content":
            "Напиши полный код на Python (с сопротивлением воздуха) для "
            "моделирования движения снаряда, выпущенного под углом к горизонту. "
            "Покажи только код в блоке ```python ... ```, без объяснений."},
        {"role": "assistant", "content":
            "```python\n"
            "import numpy as np\n"
            "import matplotlib.pyplot as plt\n"
            "v0 = 50.0\n"
            "theta_deg = 45.0\n"
            "g = 9.81\n"
            "# ... длинный код ...\n"
            "plt.show()\n"
            "```"},
        {"role": "user", "content":
            "Отлично! Давай расширим пример — добавим сопротивление воздуха, "
            "чтобы модель стала более реалистичной. Это уже будет настоящая "
            "баллистическая симуляция, хотя и упрощённая."},
    ],
    "stream": True,
}

print(f"[{datetime.now().isoformat()}] Sending 2nd-question stream to balancer...")
print(f"  Payload: {len(json.dumps(body))} bytes")
print()

start = time.time()
chunks = []
content_parts = []
done_flag = False
done_reason = None
error_msg = None
done_chunk_raw = None

try:
    # Collect all raw lines too for diagnostics
    raw_chunks_dump = []

    with requests.post(
        f"{BALANCER}/api/chat",
        json=body,
        headers={"X-API-Token": API_TOKEN},
        stream=True,
        timeout=300,
    ) as resp:
        if resp.status_code != 200:
            print(f"  HTTP {resp.status_code}: {resp.text[:500]}")
            sys.exit(1)

        for line in resp.iter_lines(decode_unicode=False):  # keep as bytes for full fidelity
            now = time.time()
            if not line:
                continue
            raw_chunks_dump.append(line)
            try:
                line_str = line.decode("utf-8", errors="replace")
            except:
                line_str = str(line[:200])
            chunks.append(line_str)
            try:
                obj = json.loads(line_str)
                msg = obj.get("message", {})
                content = msg.get("content", "")
                if content:
                    content_parts.append(content)
                if obj.get("done"):
                    done_flag = True
                    done_reason = obj.get("done_reason")
                    done_chunk_raw = line_str
                    # Log last 20 chunks when done arrives
                    if len(chunks) > 0:
                        print(f"  [DONE at chunk #{len(chunks)} @ {now - start:.2f}s] done_reason={done_reason}, content_len={sum(len(p) for p in content_parts)}")
                if obj.get("error"):
                    error_msg = obj["error"]
                    print(f"  [chunk #{len(chunks)} @ {now - start:.2f}s] ERROR in chunk: {obj['error']}")
            except json.JSONDecodeError:
                # Could be the final [DONE] line or a malformed chunk
                if b"DONE" in line:
                    print(f"  [chunk #{len(chunks)} @ {now - start:.2f}s] [DONE] SSE terminator")
                else:
                    print(f"  [chunk #{len(chunks)} @ {now - start:.2f}s] non-JSON: {line_str[:200]}")
except requests.exceptions.RequestException as e:
    print(f"  RequestException after {len(chunks)} chunks: {e}")

elapsed = time.time() - start
full_content = "".join(content_parts)

print("=" * 60)
print("RESULT")
print("=" * 60)
print(f"Total chunks:       {len(chunks)}")
print(f"Total content chars: {len(full_content)}")
print(f"Elapsed:            {elapsed:.2f}s")
print(f"Done flag:          {done_flag}")
print(f"Done reason:        {done_reason}")
print(f"Error:              {error_msg}")
print()

# ===== ANALYSIS: closing backticks =====
backtick_count = full_content.count("```")
print(f"Backticks in content: {backtick_count}")
if backtick_count % 2 != 0:
    print(f"  ⚠️  ODD backtick count — code block NOT closed!")
else:
    print(f"  ✓ Even count — blocks properly closed")

# Check for code block structure
import re
opens = re.findall(r'```\w*\n', full_content)
closes = re.findall(r'\n```', full_content)
print(f"Code block opens:  {len(opens)}")
print(f"Code block closes: {len(closes)}")
if len(opens) != len(closes):
    print(f"  ⚠️  MISMATCH: {len(opens)} open vs {len(closes)} close")

# ===== ANALYSIS: garbled UTF-8 at end =====
print()
print("Last 300 chars of content:")
print(repr(full_content[-300:]))
print()

# Detect "diamonds" or other rendering artifacts
diamond_count = full_content.count("♦")
question_block = full_content.count("�")
print(f"Diamond (♦) chars: {diamond_count}")
print(f"Replacement char (�): {question_block}")
if diamond_count > 0 or question_block > 0:
    print(f"  ⚠️  Found rendering artifacts at end of response!")

# Check last 50 bytes as raw
print()
print("Last 50 bytes (hex):")
for i, b in enumerate(full_content[-50:].encode("utf-8")):
    print(f"  {b:02x} ({chr(b) if 32 <= b < 127 else '?'})", end="")
    if (i + 1) % 16 == 0:
        print()
print()
print()

# ===== ANALYSIS: done chunk =====
if done_chunk_raw:
    print("Final 'done' chunk raw:")
    print(f"  {done_chunk_raw[:500]}")
    print()
    # Check if done message has content with closing backticks
    try:
        done_obj = json.loads(done_chunk_raw)
        done_msg_content = done_obj.get("message", {}).get("content", "")
        print(f"  Done chunk message.content: {len(done_msg_content)} chars")
        if done_msg_content:
            print(f"  Last 200: {repr(done_msg_content[-200:])}")
    except:
        pass

# Save full content (unique file per run)
import os
os.makedirs("_diag", exist_ok=True)
out_path = rf"C:\Ollama\ollamalegion\_diag\openwebui_closing_backticks_{int(time.time())}.txt"
with open(out_path, "w", encoding="utf-8") as f:
    f.write(full_content)
print(f"Full response saved to {out_path}")

# Also save raw chunks for bad runs
if backtick_count % 2 != 0 or error_msg:
    raw_path = rf"C:\Ollama\ollamalegion\_diag\openwebui_closing_backticks_raw_{int(time.time())}.txt"
    with open(raw_path, "wb") as f:
        for raw_line in raw_chunks_dump:
            f.write(raw_line + b"\n")
    print(f"RAW chunks saved to {raw_path} ({len(raw_chunks_dump)} chunks, {sum(len(l) for l in raw_chunks_dump)} bytes)")
