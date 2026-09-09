#!/usr/bin/env python3
"""R60.25 test: long code generation through balancer with auto-continue (STREAMING)."""
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
    "stream": True,  # OpenWebUI uses streaming
    "max_tokens": 4096,
    "temperature": 0.7,
}

print(f"Sending {len(PROMPT)} char prompt to {BALANCER} (streaming, R60.25)")
start = time.time()
try:
    r = requests.post(
        f"{BALANCER}/v1/chat/completions",
        json=body,
        timeout=900,  # 15 min
        stream=True,  # python-side streaming
    )
    elapsed = time.time() - start
    print(f"HTTP {r.status_code}, time {elapsed:.1f}s")

    content_parts = []
    last_data = None
    for line in r.iter_lines(decode_unicode=True):
        if not line:
            continue
        if line.startswith("data: "):
            data_str = line[6:].strip()
            if data_str == "[DONE]":
                break
            try:
                data = json.loads(data_str)
                last_data = data
                choices = data.get("choices", [])
                if choices:
                    delta = choices[0].get("delta", {})
                    if "content" in delta:
                        content_parts.append(delta["content"])
            except json.JSONDecodeError:
                pass

    elapsed = time.time() - start
    content = "".join(content_parts)
    print(f"Stream done, total time {elapsed:.1f}s")
    print(f"Content length: {len(content)} chars")
    print()

    if last_data:
        choices = last_data.get("choices", [{}])
        finish_reason = choices[0].get("finish_reason", "?")
        usage = last_data.get("usage", {})
        print(f"final finish_reason: {finish_reason}")
        print(f"prompt_tokens: {usage.get('prompt_tokens', '?')}")
        print(f"completion_tokens: {usage.get('completion_tokens', '?')}")
    print()
    print("---LAST 800 CHARS OF CONTENT---")
    print(content[-800:])
    print("---END---")
    print()

    # Truncation checks
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
        print("✅ NO TRUNCATION INDICATORS")

    if "<|im_end|>" in content:
        count = content.count("<|im_end|>")
        print(f"  ⚠️ <|im_end|> appears {count} times in content (ChatML-EOS mid-generation)")

    outfile = "_diag/last_r60_25_streaming_response.txt"
    with open(outfile, "w", encoding="utf-8") as f:
        f.write(f"=== R60.25 STREAMING test: {PROMPT[:80]}... ===\n")
        f.write(f"Total time: {elapsed:.1f}s\n")
        f.write(f"Content length: {len(content)}\n\n")
        f.write(content)
    print(f"\nSaved to {outfile}")
except Exception as e:
    elapsed = time.time() - start
    print(f"EXCEPTION after {elapsed:.1f}s: {e}")
