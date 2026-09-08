#!/usr/bin/env python3
"""R60.20b — Reproduce Qwen3-Instruct-2507-q4km "diamond bullets" pattern.

User's screenshot showed: "♦♦♦♦ Добавить сопротивление воздуха?" etc.
— model generating decorative ♦♦♦♦ as bullet markers.

Hypothesis: Qwen3-Instruct-2507-q4km uses ♦ as bullet/separator
in certain conversational contexts (gemma-4 does NOT do this).

This script sends a similar "Хочешь?" question and counts ♦ chars
in response.
"""
import json
import time
import requests
from datetime import datetime

BALANCER = "http://localhost:18080"
API_TOKEN = "changeme-bundled-with-agent-token"
MODEL = "Qwen3-Instruct-2507-q4km"

# Similar to user's context: "Отлично! Давай расширим пример — добавим
# сопротивление воздуха..." then ask if they want follow-ups.
body = {
    "model": MODEL,
    "messages": [
        {"role": "user", "content":
            "Напиши полный код на Python (с сопротивлением воздуха) для "
            "моделирования движения снаряда. Покажи только код, без объяснений."},
        {"role": "assistant", "content":
            "```python\n"
            "import numpy as np\n"
            "import matplotlib.pyplot as plt\n"
            "v0 = 50.0\n"
            "theta_deg = 45.0\n"
            "g = 9.81\n"
            "k = 0.015\n"
            "m = 1.0\n"
            "t_max = 10.0\n"
            "dt = 0.01\n"
            "t = np.arange(0, t_max, dt)\n"
            "vx = np.zeros_like(t)\n"
            "vy = np.zeros_like(t)\n"
            "x = np.zeros_like(t)\n"
            "y = np.zeros_like(t)\n"
            "# ... full code ...\n"
            "plt.show()\n"
            "```"},
        {"role": "user", "content":
            "Отлично! Давай расширим пример — добавим сопротивление воздуха, "
            "чтобы модель стала более реалистичной. Это уже будет настоящая "
            "баллистическая симуляция, хотя и упрощённая."},
    ],
    "stream": True,
}

print(f"[{datetime.now().isoformat()}] Sending 2nd-question with 'Хочешь?' trigger...")
print()

start = time.time()
chunks = []
content_parts = []
done_flag = False
done_chunk_raw = None

try:
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

        for line in resp.iter_lines(decode_unicode=True):
            if not line:
                continue
            chunks.append(line)
            try:
                obj = json.loads(line)
                msg = obj.get("message", {})
                content = msg.get("content", "")
                if content:
                    content_parts.append(content)
                if obj.get("done"):
                    done_flag = True
                    done_chunk_raw = line
            except json.JSONDecodeError:
                pass
except requests.exceptions.RequestException as e:
    print(f"  RequestException: {e}")

elapsed = time.time() - start
full_content = "".join(content_parts)

print("=" * 60)
print("RESULT")
print("=" * 60)
print(f"Total chunks:       {len(chunks)}")
print(f"Total content chars: {len(full_content)}")
print(f"Elapsed:            {elapsed:.2f}s")
print(f"Done:               {done_flag}")
print()

# Analyze ♦ pattern
print("Diamond (♦ U+2666) analysis:")
print(f"  Total ♦ chars:  {full_content.count('♦')}")
print(f"  Max run length:  ", end="")
max_run = 0
cur_run = 0
for ch in full_content:
    if ch == "♦":
        cur_run += 1
        max_run = max(max_run, cur_run)
    else:
        cur_run = 0
print(max_run)
print()

# Print content with ♦ visible
print("Full content (♦ visible):")
print(full_content)
print()

# Save to file
import os
os.makedirs("_diag", exist_ok=True)
out_path = rf"C:\Ollama\ollamalegion\_diag\diamond_bullets_{int(time.time())}.txt"
with open(out_path, "w", encoding="utf-8") as f:
    f.write(full_content)
print(f"Saved to {out_path}")
