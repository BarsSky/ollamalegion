#!/usr/bin/env python3
"""R60.25 test: long code generation through balancer with auto-continue."""
import json
import time
import sys
import requests

BALANCER = "http://localhost:18080"
MODEL = "Qwen3-Instruct-2507-q4km"
PROMPT = """Напиши HTML страницу с JavaScript для расчёта траектории полёта снаряда.
В форме пользователь вводит:
- начальную скорость (м/с)
- угол броска (градусы)
- высоту (м)
- гравитацию (по умолчанию 9.81)

Нужно рассчитать:
- максимальное время полёта
- максимальную дальность
- максимальную высоту
- траекторию (массив точек x,y)
- отрисовать траекторию на canvas

Код должен быть полным, рабочим, с красивым CSS. Покажи весь код целиком, не обрывай."""

body = {
    "model": MODEL,
    "messages": [{"role": "user", "content": PROMPT}],
    "stream": False,
    "max_tokens": 4096,
    "temperature": 0.7,
}

print(f"Sending {len(PROMPT)} char prompt to {BALANCER} (R60.25 auto-continue should fire)")
start = time.time()
try:
    r = requests.post(
        f"{BALANCER}/v1/chat/completions",
        json=body,
        timeout=900,  # 15 min — long enough for original + auto-continue
    )
    elapsed = time.time() - start
    print(f"HTTP {r.status_code}, time {elapsed:.1f}s")
    if r.status_code != 200:
        print(f"Body: {r.text[:500]}")
        sys.exit(1)
    data = r.json()
    content = data["choices"][0]["message"]["content"]
    finish_reason = data["choices"][0]["finish_reason"]
    usage = data.get("usage", {})

    print(f"finish_reason: {finish_reason}")
    print(f"prompt_tokens: {usage.get('prompt_tokens')}")
    print(f"completion_tokens: {usage.get('completion_tokens')}")
    print(f"total_tokens: {usage.get('total_tokens')}")
    print(f"Content length: {len(content)} chars")
    print()
    print("---LAST 800 CHARS OF CONTENT---")
    print(content[-800:])
    print("---END---")
    print()

    # Truncation checks
    last_400 = content[-400:]
    truncated_indicators = []
    open_ticks = content.count("```")
    if open_ticks % 2 != 0:
        truncated_indicators.append(f"unclosed code block: {open_ticks} backticks")
    if "```" in content:
        last_code_block = content.rsplit("```", -2)[-1] if content.count("```") >= 2 else ""
        opens = last_code_block.count("{")
        closes = last_code_block.count("}")
        if opens > closes:
            truncated_indicators.append(f"unclosed braces in last code block: {opens} open vs {closes} close")
    last_50 = content[-50:].rstrip()
    if last_50 and not last_50.endswith((".", "!", "?", "`", "}", ";", "</html>", "</script>")):
        if last_50[-1].isalnum() or last_50[-1] in (",", "(", "+", "-", "*", "/", "="):
            truncated_indicators.append(f"mid-line cutoff: ends with '{last_50[-20:]}'")

    if truncated_indicators:
        print("TRUNCATION INDICATORS:")
        for ind in truncated_indicators:
            print(f"  ⚠️ {ind}")
    else:
        print("✅ NO TRUNCATION INDICATORS — auto-continue worked!")

    outfile = "_diag/last_r60_25_trajectory_response.txt"
    with open(outfile, "w", encoding="utf-8") as f:
        f.write(f"=== R60.25 test: {PROMPT[:80]}... ===\n")
        f.write(f"HTTP {r.status_code}, {elapsed:.1f}s\n")
        f.write(f"finish_reason: {finish_reason}\n")
        f.write(f"prompt_tokens: {usage.get('prompt_tokens')}\n")
        f.write(f"completion_tokens: {usage.get('completion_tokens')}\n")
        f.write(f"content_length: {len(content)}\n\n")
        f.write(content)
    print(f"\nSaved full response to {outfile}")
except Exception as e:
    elapsed = time.time() - start
    print(f"EXCEPTION after {elapsed:.1f}s: {e}")
