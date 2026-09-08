#!/usr/bin/env python3
"""Reproduce OpenWebUI 2nd-question cutoff through balancer /api/chat.

Sends a 3-message conversation (assistant history + new question) as a
streaming request to the balancer, counts chunks, captures timing,
and saves the full response for inspection.
"""
import json
import sys
import time
import requests
from datetime import datetime

BALANCER = "http://localhost:18080"
API_TOKEN = "changeme-bundled-with-agent-token"
MODEL = "Qwen3-Instruct-2507-q4km"

# 3-message history simulating "2nd question in session"
# (matches the prompt_tail we saw in last-prompt: projectile + follow-up)
body = {
    "model": MODEL,
    "messages": [
        {"role": "user", "content":
            "Напиши полный код на Python (с сопротивлением воздуха) для "
            "моделирования движения снаряда, выпущенного под углом к горизонту. "
            "Покажи только код, без объяснений."},
        {"role": "assistant", "content":
            "```python\n"
            "import numpy as np\n"
            "import matplotlib.pyplot as plt\n\n"
            "# Параметры\n"
            "v0 = 50.0\n"
            "theta_deg = 45.0\n"
            "g = 9.81\n\n"
            "# Конвертация угла\n"
            "theta_rad = np.radians(theta_deg)\n\n"
            "# Начальные условия\n"
            "vx0 = v0 * np.cos(theta_rad)\n"
            "vy0 = v0 * np.sin(theta_rad)\n\n"
            "# Параметры сопротивления воздуха\n"
            "k = 0.015\n"
            "m = 1.0\n\n"
            "# Время моделирования\n"
            "t_max = 10.0\n"
            "dt = 0.01\n\n"
            "# Инициализация массивов\n"
            "t = np.arange(0, t_max, dt)\n"
            "vx = np.zeros_like(t)\n"
            "vy = np.zeros_like(t)\n"
            "x = np.zeros_like(t)\n"
            "y = np.zeros_like(t)\n\n"
            "vx[0] = vx0\n"
            "vy[0] = vy0\n\n"
            "# Численное интегрирование (метод Эйлера с учётом сопротивления)\n"
            "for i in range(1, len(t)):\n"
            "    v = np.sqrt(vx[i-1]**2 + vy[i-1]**2)\n"
            "    ax = -k/m * v * vx[i-1]\n"
            "    ay = -g - k/m * v * vy[i-1]\n"
            "    vx[i] = vx[i-1] + ax * dt\n"
            "    vy[i] = vy[i-1] + ay * dt\n"
            "    x[i] = x[i-1] + vx[i] * dt\n"
            "    y[i] = y[i-1] + vy[i] * dt\n"
            "    if y[i] < 0:\n"
            "        break\n\n"
            "# Построение графика\n"
            "plt.figure(figsize=(10, 6))\n"
            "plt.plot(x[:i], y[:i])\n"
            "plt.xlabel('Дальность (м)')\n"
            "plt.ylabel('Высота (м)')\n"
            "plt.title('Траектория снаряда с сопротивлением воздуха')\n"
            "plt.grid(True)\n"
            "plt.show()\n"
            "```"},
        {"role": "user", "content":
            "А можешь объяснить, как именно работает сопротивление воздуха "
            "в этом коде? Что делает параметр k и почему направление силы "
            "сопротивления противоположно вектору скорости? Дай развёрнутый ответ."},
    ],
    "stream": True,
}

print(f"[{datetime.now().isoformat()}] Sending 2nd-question stream to balancer...")
print(f"  Payload: {len(json.dumps(body))} bytes")
print(f"  Messages: {len(body['messages'])}")
print()

start = time.time()
chunks = []
content_parts = []
last_done = False
error_seen = None
last_chunk_at = None

try:
    with requests.post(
        f"{BALANCER}/api/chat",
        json=body,
        headers={"X-API-Token": API_TOKEN},
        stream=True,
        timeout=300,
    ) as resp:
        print(f"[{datetime.now().isoformat()}] HTTP {resp.status_code}, headers:")
        for k, v in resp.headers.items():
            print(f"  {k}: {v}")
        print()

        if resp.status_code != 200:
            print(f"  body: {resp.text[:500]}")
            sys.exit(1)

        for line in resp.iter_lines(decode_unicode=True):
            now = time.time()
            elapsed = now - start
            last_chunk_at = now
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
                    last_done = True
                    if obj.get("done_reason"):
                        print(f"  [chunk #{len(chunks)} @ {elapsed:.2f}s] done_reason={obj['done_reason']}")
                if obj.get("error"):
                    error_seen = obj["error"]
                    print(f"  [chunk #{len(chunks)} @ {elapsed:.2f}s] ERROR: {obj['error']}")
            except json.JSONDecodeError:
                print(f"  [chunk #{len(chunks)} @ {elapsed:.2f}s] not JSON: {line[:100]}")

            if len(chunks) % 20 == 0:
                full = "".join(content_parts)
                tail = full[-100:].replace("\n", "\\n")
                print(f"  [chunk #{len(chunks)} @ {elapsed:.2f}s] tail=...{tail!r}")

except requests.exceptions.ChunkedEncodingError as e:
    print(f"[{datetime.now().isoformat()}] ChunkedEncodingError after {len(chunks)} chunks: {e}")
except requests.exceptions.RequestException as e:
    print(f"[{datetime.now().isoformat()}] RequestException after {len(chunks)} chunks: {e}")
except KeyboardInterrupt:
    print(f"[{datetime.now().isoformat()}] Interrupted after {len(chunks)} chunks")

elapsed = time.time() - start
full_content = "".join(content_parts)

print()
print("=" * 60)
print("RESULT")
print("=" * 60)
print(f"Total chunks:       {len(chunks)}")
print(f"Total content chars: {len(full_content)}")
print(f"Last chunk @:        {f'{(last_chunk_at - start):.2f}s' if last_chunk_at else 'N/A'}")
print(f"Total elapsed:       {elapsed:.2f}s")
print(f"Last done flag:      {last_done}")
print(f"Error in response:   {error_seen}")
print()
print("Last 400 chars of content:")
print(full_content[-400:])
print()
print("First 200 chars of content:")
print(full_content[:200])
print()
# Save full content to file for inspection
with open(r"C:\Ollama\ollamalegion\_diag\openwebui_2nd_response.txt", "w", encoding="utf-8") as f:
    f.write(full_content)
print(f"Full response saved to _diag/openwebui_2nd_response.txt")
