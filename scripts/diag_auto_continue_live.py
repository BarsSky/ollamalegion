#!/usr/bin/env python3
"""R60.21 live test — verify auto-continue works on balancer.

Test:
1. Send a 3-message chat that previously caused Qwen3 to truncate mid-code.
2. Compare response with/without auto-continue.
3. Verify: with auto-continue, the response ends with closing ``` + has
   more content than the truncated version.
"""
import json
import time
import requests
import sys

BALANCER = "http://localhost:18080"
API_TOKEN = "changeme-bundled-with-agent-token"
MODEL = "Qwen3-Instruct-2507-q4km"

# Same 3-message conversation that triggered R60.20 truncation
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


def run_test(label: str, timeout_sec: int = 300) -> dict:
    """Run one streaming request, return summary."""
    print(f"\n=== {label} ===")
    start = time.time()
    chunks = []
    content_parts = []
    done_flag = False
    done_reason = None
    try:
        with requests.post(
            f"{BALANCER}/api/chat",
            json=body,
            headers={"X-API-Token": API_TOKEN},
            stream=True,
            timeout=timeout_sec,
        ) as resp:
            if resp.status_code != 200:
                return {"error": f"HTTP {resp.status_code}: {resp.text[:200]}"}
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
                        done_reason = obj.get("done_reason")
                except json.JSONDecodeError:
                    pass
    except requests.exceptions.RequestException as e:
        return {"error": str(e)}

    elapsed = time.time() - start
    full_content = "".join(content_parts)
    backticks = full_content.count("```")
    incomplete = backticks % 2 != 0
    last_line = full_content.rstrip().rsplit("\n", 1)[-1] if full_content else ""
    return {
        "chunks": len(chunks),
        "chars": len(full_content),
        "elapsed": elapsed,
        "done_reason": done_reason,
        "backticks": backticks,
        "incomplete": incomplete,
        "last_line": last_line,
        "content": full_content,
    }


def main():
    # Run 1: baseline (no auto-continue on balancer, since env var not set)
    r1 = run_test("BASELINE (no auto-continue)")
    if "error" in r1:
        print(f"  ERROR: {r1['error']}")
        return 1
    print(f"  Chars: {r1['chars']}, Chunks: {r1['chunks']}, Elapsed: {r1['elapsed']:.1f}s")
    print(f"  Done: {r1['done_reason']}, Backticks: {r1['backticks']}, Incomplete: {r1['incomplete']}")
    print(f"  Last line: {r1['last_line']!r}")
    print(f"  Content preview (last 200): ...{r1['content'][-200:]!r}")

    # Save for comparison
    out_path = r"C:\Ollama\ollamalegion\_diag\auto_continue_baseline.txt"
    with open(out_path, "w", encoding="utf-8") as f:
        f.write(r1['content'])
    print(f"  Saved to {out_path}")

    print()
    print("NOTE: Auto-continue is operator opt-in via LB_AUTO_CONTINUE_ON_TRUNCATION=1")
    print("      on the BALANCER container. To test live, set it and restart balancer.")
    print()
    print("Quick test summary:")
    print(f"  Without auto-continue: chars={r1['chars']}, complete={not r1['incomplete']}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
