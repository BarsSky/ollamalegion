#!/usr/bin/env python3
"""R60.26 candidate: точная диагностика chunk timing в streaming path.

Цель: выяснить где bottleneck — в балансере, cppworker, или где-то ещё.
Меряем:
- Время до первого chunk (first-byte latency)
- Интервалы между chunks
- Real tokens/sec (из usage)
- Полноту ответа (finish_reason + content completeness)
"""
import json
import time
import sys
import requests
from datetime import datetime

BALANCER = "http://localhost:18080"
# Тот же trajectory prompt но с max_tokens=512 для быстрого теста
PROMPT = """Напиши HTML страницу с JavaScript для расчёта траектории полёта снаряда.
В форме пользователь вводит: начальную скорость (м/с), угол броска (градусы), высоту (м), гравитацию (по умолчанию 9.81).
Нужно рассчитать: максимальное время полёта, максимальную дальность, максимальную высоту, траекторию (массив точек x,y), отрисовать траекторию на canvas.
Код должен быть полным, рабочим, с красивым CSS. Покажи весь код целиком, не обрывай."""

body = {
    "model": "Qwen3-Instruct-2507-q4km",
    "messages": [{"role": "user", "content": PROMPT}],
    "stream": True,
    "max_tokens": 512,  # short for fast test
    "temperature": 0.7,
}

print(f"=== {datetime.now().isoformat()} ===")
print(f"Sending {len(PROMPT)} char prompt, max_tokens=512, streaming")
print(f"Target: {BALANCER}")
sys.stdout.flush()

start = time.time()
try:
    r = requests.post(
        f"{BALANCER}/v1/chat/completions",
        json=body,
        timeout=300,  # 5 min - should be plenty for 512 tokens
        stream=True,
    )
    first_byte_t = time.time() - start
    print(f"HTTP {r.status_code}, first byte at {first_byte_t:.2f}s")
    sys.stdout.flush()

    content_parts = []
    chunk_times = []
    last_data = None
    chunks_received = 0
    last_chunk_t = first_byte_t

    for line in r.iter_lines(decode_unicode=True):
        if not line:
            continue
        if line.startswith("data: "):
            data_str = line[6:].strip()
            if data_str == "[DONE]":
                print(f"  [DONE] marker at {time.time()-start:.2f}s")
                break
            try:
                data = json.loads(data_str)
                chunks_received += 1
                now = time.time() - start
                chunk_times.append(now)
                choices = data.get("choices", [])
                if choices:
                    delta = choices[0].get("delta", {})
                    content = delta.get("content", "")
                    if content:
                        content_parts.append(content)
                    if chunks_received <= 5 or chunks_received % 50 == 0:
                        print(f"  chunk #{chunks_received} at {now:.2f}s (+{now-last_chunk_t:.3f}s): {content[:50]!r}")
                        sys.stdout.flush()
                    last_chunk_t = now
                last_data = data
            except json.JSONDecodeError as e:
                print(f"  JSON decode error at chunk #{chunks_received}: {e}")
                print(f"  Raw line: {data_str[:200]!r}")

    elapsed = time.time() - start
    full_content = "".join(content_parts)
    finish_reason = last_data.get("choices", [{}])[0].get("finish_reason") if last_data else "?"
    usage = last_data.get("usage", {}) if last_data else {}

    print()
    print(f"=== SUMMARY ===")
    print(f"Total time: {elapsed:.2f}s")
    print(f"First byte: {first_byte_t:.2f}s")
    print(f"Chunks received: {chunks_received}")
    print(f"Content length: {len(full_content)} chars")
    print(f"finish_reason: {finish_reason}")
    print(f"usage: {usage}")
    if usage:
        completion_tokens = usage.get("completion_tokens", 0)
        if completion_tokens > 0 and elapsed > 0:
            tps = completion_tokens / elapsed
            print(f"Tokens/sec (overall): {tps:.2f}")
    if len(chunk_times) > 1:
        intervals = [chunk_times[i+1] - chunk_times[i] for i in range(len(chunk_times)-1)]
        avg_int = sum(intervals) / len(intervals)
        max_int = max(intervals)
        print(f"Chunk interval avg: {avg_int*1000:.1f}ms, max: {max_int*1000:.1f}ms")
    print()
    print("---FIRST 300 chars---")
    print(full_content[:300])
    print("---LAST 300 chars---")
    print(full_content[-300:])
    print("---END---")
    sys.stdout.flush()

    # Truncation checks
    last_100 = full_content[-100:].rstrip()
    open_ticks = full_content.count("```")
    issues = []
    if open_ticks % 2 != 0:
        issues.append(f"unclosed code blocks: {open_ticks} backticks")
    if finish_reason == "length":
        issues.append("finish_reason=length (truncated by max_tokens)")
    elif finish_reason == "stop":
        print("✅ finish_reason=stop (clean completion)")
    if last_100 and not last_100.endswith((".", "!", "?", "`", "}", ";", "</html>", "</script>")):
        if last_100[-1].isalnum() or last_100[-1] in (",", "(", "+", "-", "*", "/", "="):
            issues.append(f"mid-line cutoff: ...{last_100[-30:]!r}")
    if issues:
        print("ISSUES:", "; ".join(issues))
    else:
        print("✅ NO ISSUES — response looks complete")

except requests.exceptions.Timeout as e:
    elapsed = time.time() - start
    print(f"❌ TIMEOUT after {elapsed:.1f}s: {e}")
except Exception as e:
    elapsed = time.time() - start
    print(f"❌ EXCEPTION after {elapsed:.1f}s: {type(e).__name__}: {e}")
