#!/usr/bin/env python3
"""R60.20b v2 — Try harder to trigger Qwen3 diamond bullets.

User's screenshot showed: "Хочешь: ♦♦♦♦ Добавить сопротивление воздуха?" etc.
Need to provoke this style of response.

Strategy: ask open-ended question that requires clarification.
Different prompts to test:
1. "Что дальше делать?" (what to do next?)
2. Short acknowledgment + question
3. Ask the model to "explain more"
"""
import json
import time
import requests

BALANCER = "http://localhost:18080"
API_TOKEN = "changeme-bundled-with-agent-token"
MODEL = "Qwen3-Instruct-2507-q4km"

prompts = [
    # Test 1: "да/нет" question that needs clarification
    {
        "name": "yes_no_with_code",
        "messages": [
            {"role": "user", "content": "Напиши код для движения снаряда с сопротивлением воздуха."},
            {"role": "assistant", "content": "```python\nimport numpy as np\nprint('hello')\n```"},
            {"role": "user", "content": "А теперь объясни, как это работает."},
        ],
    },
    # Test 2: "хочешь?" trigger with "да"
    {
        "name": "want_trigger",
        "messages": [
            {"role": "user", "content": "Хочешь, я напишу код для движения снаряда?"},
            {"role": "assistant", "content": "Да, конечно!"},
            {"role": "user", "content": "Тогда добавь сопротивление воздуха."},
        ],
    },
    # Test 3: "Отлично! Давай расширим" + "Хочешь?" pattern from user
    {
        "name": "user_pattern",
        "messages": [
            {"role": "user", "content": "Напиши код для движения снаряда."},
            {"role": "assistant", "content": "```python\nimport numpy as np\nimport matplotlib.pyplot as plt\nv0 = 50.0\ntheta_deg = 45.0\ng = 9.81\n```"},
            {"role": "user", "content": "Отлично! Давай расширим пример — добавим сопротивление воздуха, чтобы модель стала более реалистичной."},
        ],
    },
    # Test 4: open-ended follow-up after code
    {
        "name": "followup_options",
        "messages": [
            {"role": "user", "content": "Напиши код для баллистической траектории."},
            {"role": "assistant", "content": "```python\n# код\n```"},
            {"role": "user", "content": "Спасибо! А что ещё можно добавить?"},
        ],
    },
    # Test 5: Direct ask for multiple options
    {
        "name": "explicit_options",
        "messages": [
            {"role": "user", "content": "Напиши код для движения снаряда и предложи несколько улучшений."},
        ],
    },
]

for prompt in prompts:
    print(f"\n=== Test: {prompt['name']} ===")
    body = {
        "model": MODEL,
        "messages": prompt["messages"],
        "stream": True,
    }
    start = time.time()
    content_parts = []
    try:
        with requests.post(
            f"{BALANCER}/api/chat",
            json=body,
            headers={"X-API-Token": API_TOKEN},
            stream=True,
            timeout=180,
        ) as resp:
            if resp.status_code != 200:
                print(f"  HTTP {resp.status_code}")
                continue
            for line in resp.iter_lines(decode_unicode=True):
                if not line:
                    continue
                try:
                    obj = json.loads(line)
                    msg = obj.get("message", {})
                    content = msg.get("content", "")
                    if content:
                        content_parts.append(content)
                except json.JSONDecodeError:
                    pass
    except requests.exceptions.RequestException as e:
        print(f"  Error: {e}")
        continue

    elapsed = time.time() - start
    full_content = "".join(content_parts)
    diamond_count = full_content.count("♦")

    print(f"  Chars: {len(full_content)}, Elapsed: {elapsed:.1f}s, ♦: {diamond_count}")
    if diamond_count > 0:
        print(f"  Content preview:")
        # Show first 800 chars
        preview = full_content[:800]
        print("    " + preview.replace("\n", "\n    "))
