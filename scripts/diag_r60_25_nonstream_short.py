#!/usr/bin/env python3
"""R60.26 candidate: non-streaming path с коротким max_tokens=512.

Если streaming за 85s проходит чисто, а non-streaming висит 900s,
проблема в non-streaming path балансера (буферизация всего response).
"""
import json
import time
import sys
import requests
from datetime import datetime

BALANCER = "http://localhost:18080"
PROMPT = """Напиши HTML страницу с JavaScript для расчёта траектории полёта снаряда.
В форме пользователь вводит: начальную скорость (м/с), угол броска (градусы), высоту (м), гравитацию (по умолчанию 9.81).
Нужно рассчитать: максимальное время полёта, максимальную дальность, максимальную высоту, траекторию (массив точек x,y), отрисовать траекторию на canvas.
Код должен быть полным, рабочим, с красивым CSS. Покажи весь код целиком, не обрывай."""

body = {
    "model": "Qwen3-Instruct-2507-q4km",
    "messages": [{"role": "user", "content": PROMPT}],
    "stream": False,  # <-- key difference
    "max_tokens": 512,
    "temperature": 0.7,
}

print(f"=== {datetime.now().isoformat()} ===")
print(f"Non-streaming, max_tokens=512, target={BALANCER}")
sys.stdout.flush()

start = time.time()
try:
    r = requests.post(
        f"{BALANCER}/v1/chat/completions",
        json=body,
        timeout=300,
    )
    elapsed = time.time() - start
    print(f"HTTP {r.status_code}, full response at {elapsed:.2f}s")
    sys.stdout.flush()

    if r.status_code != 200:
        print(f"Body: {r.text[:1000]}")
        sys.exit(1)

    data = r.json()
    content = data["choices"][0]["message"]["content"]
    finish_reason = data["choices"][0]["finish_reason"]
    usage = data.get("usage", {})

    print(f"finish_reason: {finish_reason}")
    print(f"prompt_tokens: {usage.get('prompt_tokens')}")
    print(f"completion_tokens: {usage.get('completion_tokens')}")
    print(f"total_tokens: {usage.get('total_tokens')}")
    print(f"content length: {len(content)} chars")
    print(f"duration_ms (if present): {data.get('duration_ms')}")
    print()
    print("---FIRST 300 chars---")
    print(content[:300])
    print("---LAST 300 chars---")
    print(content[-300:])
    print("---END---")
    sys.stdout.flush()

    if usage.get("completion_tokens", 0) > 0 and elapsed > 0:
        tps = usage["completion_tokens"] / elapsed
        print(f"Overall tokens/sec: {tps:.2f}")

    if finish_reason == "stop":
        print("✅ finish_reason=stop (clean)")
    elif finish_reason == "length":
        print(f"⚠️ finish_reason=length (truncated at {usage.get('completion_tokens')} tokens)")

    if elapsed < 100 and finish_reason == "stop":
        print("✅ Non-streaming ALSO works fast!")
    elif elapsed > 100:
        print(f"⚠️ Non-streaming took {elapsed:.1f}s — significantly slower than streaming (85s)")

except requests.exceptions.Timeout as e:
    elapsed = time.time() - start
    print(f"❌ TIMEOUT after {elapsed:.1f}s: {e}")
except Exception as e:
    elapsed = time.time() - start
    print(f"❌ EXCEPTION after {elapsed:.1f}s: {type(e).__name__}: {e}")
