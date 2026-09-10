#!/usr/bin/env python3
"""R60.26 hypothesis test: max_tokens=2048 should fit under requestTimeout=600s.

If 2048 tokens take ~400s (5 tok/s), 600s timeout leaves 200s buffer.
If response is clean, then requestTimeout is the real bottleneck for max_tokens=4096.
If response is also truncated, then something else is wrong.
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
    "stream": False,
    "max_tokens": 2048,  # ~400s @ 5 tok/s, fits under 600s requestTimeout
    "temperature": 0.7,
}

print(f"=== {datetime.now().isoformat()} ===")
print(f"Non-streaming, max_tokens=2048, target={BALANCER}")
sys.stdout.flush()

start = time.time()
try:
    r = requests.post(
        f"{BALANCER}/v1/chat/completions",
        json=body,
        timeout=600,
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
    print(f"duration_ms: {data.get('duration_ms')}")
    print()
    print("---LAST 400 chars---")
    print(content[-400:])
    print("---END---")
    sys.stdout.flush()

    if usage.get("completion_tokens", 0) > 0 and elapsed > 0:
        tps = usage["completion_tokens"] / elapsed
        print(f"Overall tokens/sec: {tps:.2f}")

    if finish_reason == "stop":
        print("✅ finish_reason=stop (clean completion)")
    elif finish_reason == "length":
        print(f"⚠️ finish_reason=length — TRUNCATED at {usage.get('completion_tokens')} tokens (not 2048)")

    open_ticks = content.count("```")
    if open_ticks % 2 != 0:
        print(f"⚠️ Unclosed code blocks: {open_ticks} backticks (odd)")

except requests.exceptions.Timeout as e:
    elapsed = time.time() - start
    print(f"❌ TIMEOUT after {elapsed:.1f}s: {e}")
except Exception as e:
    elapsed = time.time() - start
    print(f"❌ EXCEPTION after {elapsed:.1f}s: {type(e).__name__}: {e}")
