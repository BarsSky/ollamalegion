#!/usr/bin/env python3
"""
Углублённое тестирование streaming OllamaLegion:
большие ответы, сравнение форматов DIRECT vs BALANCER, обрыв соединения,
таймауты, non-streaming с большим ответом, /v1/models, /api/tags.

Примеры:
    python scripts/e2e_streaming_deep.py
    python scripts/e2e_streaming_deep.py --balancer http://localhost:18080 \\
        --cppworker http://localhost:18093 --model llama3.1:8b
"""
import argparse
import os
import sys
import time

import requests


# ============================================================
# Аргументы / ENV
# ============================================================

def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="Углублённое тестирование streaming OllamaLegion")
    p.add_argument(
        "--balancer",
        default=os.environ.get("OLLAMALEGION_BALANCER", "http://localhost:18080"),
        help="URL балансировщика (env: OLLAMALEGION_BALANCER)",
    )
    p.add_argument(
        "--cppworker",
        default=os.environ.get("OLLAMALEGION_CPPWORKER", "http://localhost:18093"),
        help="URL CppWorker напрямую (env: OLLAMALEGION_CPPWORKER)",
    )
    p.add_argument(
        "--model",
        default=os.environ.get("OLLAMALEGION_MODEL", "gemma-4-E4B-it-Q4_K_M"),
        help="Имя тестовой модели (env: OLLAMALEGION_MODEL)",
    )
    p.add_argument(
        "--timeout",
        type=int,
        default=int(os.environ.get("OLLAMALEGION_TIMEOUT", "120")),
        help="Таймаут по умолчанию (env: OLLAMALEGION_TIMEOUT)",
    )
    return p.parse_args()


ARGS = None
BALANCER = ""
CPPWORKER = ""
MODEL = ""


def test_large_response() -> bool:
    print("=" * 60)
    print("[TEST] Large response /api/generate (check for truncation)")
    try:
        r = requests.post(
            f"{BALANCER}/api/generate",
            json={"model": MODEL,
                  "prompt": "Напиши подробное эссе на 500 слов о квантовых вычислениях.",
                  "stream": True},
            stream=True, timeout=ARGS.timeout,
        )
    except Exception as e:
        print(f"  ERROR: {e}")
        return False

    print(f"Status: {r.status_code}, Content-Type: {r.headers.get('Content-Type','')}")

    chunks = 0
    full_text = ""
    done_data = None

    for line in r.iter_lines(decode_unicode=True):
        if line:
            chunks += 1
            try:
                d = __import__("json").loads(line)
                if d.get("done"):
                    done_data = d
                if d.get("response"):
                    full_text += d["response"]
            except Exception:
                pass

    print(f"Chunks: {chunks}, Response: {len(full_text)} chars")
    print(f"First 100: {full_text[:100]}")
    print(f"Last 100:  {full_text[-100:]}")

    if done_data:
        print(f"DONE: eval_count={done_data.get('eval_count','?')}, "
              f"total_duration={done_data.get('total_duration','?')}")
    else:
        print("FAIL: No done event!")
    print()
    return done_data is not None and len(full_text) > 0


def test_format_comparison() -> bool:
    print("=" * 60)
    print("[TEST] Format comparison DIRECT vs BALANCER")

    import json

    # Direct cppworker
    rd = requests.post(
        f"{CPPWORKER}/api/generate",
        json={"model": MODEL, "prompt": "X", "stream": True},
        stream=True, timeout=30,
    )
    direct_lines = [l for l in rd.iter_lines(decode_unicode=True) if l]
    print(f"DIRECT /api/generate:")
    print(f"  Lines: {len(direct_lines)}, CT: {rd.headers.get('Content-Type','')}")
    if direct_lines:
        print(f"  First: {direct_lines[0][:100]}")
        print(f"  Last:  {direct_lines[-1][:150]}")

    # Balancer
    rb = requests.post(
        f"{BALANCER}/api/generate",
        json={"model": MODEL, "prompt": "X", "stream": True},
        stream=True, timeout=30,
    )
    bal_lines = [l for l in rb.iter_lines(decode_unicode=True) if l]
    print(f"BALANCER /api/generate:")
    print(f"  Lines: {len(bal_lines)}, CT: {rb.headers.get('Content-Type','')}")
    if bal_lines:
        print(f"  First: {bal_lines[0][:100]}")
        print(f"  Last:  {bal_lines[-1][:150]}")

    print(f"\n  Lines match: {len(direct_lines) == len(bal_lines)}")
    print()
    return True


def test_sse_format_comparison() -> bool:
    print("=" * 60)
    print("[TEST] SSE Format comparison DIRECT vs BALANCER")

    import json

    rd = requests.post(
        f"{CPPWORKER}/v1/chat/completions",
        json={"model": MODEL,
              "messages": [{"role": "user", "content": "Hi"}],
              "stream": True},
        stream=True, timeout=30,
    )
    d_lines = [l for l in rd.iter_lines(decode_unicode=True) if l]
    d_done = [l for l in d_lines if "[DONE]" in l]
    print(f"DIRECT /v1/chat/completions:")
    print(f"  Lines: {len(d_lines)}, [DONE]: {len(d_done)}, CT: {rd.headers.get('Content-Type','')}")
    if d_lines:
        print(f"  First: {d_lines[0][:100]}")
        print(f"  Last:  {d_lines[-1][:150] if d_lines[-1] else 'EMPTY'}")

    rb = requests.post(
        f"{BALANCER}/v1/chat/completions",
        json={"model": MODEL,
              "messages": [{"role": "user", "content": "Hi"}],
              "stream": True},
        stream=True, timeout=30,
    )
    b_lines = [l for l in rb.iter_lines(decode_unicode=True) if l]
    b_done = [l for l in b_lines if "[DONE]" in l]
    print(f"BALANCER /v1/chat/completions:")
    print(f"  Lines: {len(b_lines)}, [DONE]: {len(b_done)}, CT: {rb.headers.get('Content-Type','')}")
    if b_lines:
        print(f"  First: {b_lines[0][:100]}")
        print(f"  Last:  {b_lines[-1][:150] if b_lines[-1] else 'EMPTY'}")

    print(f"\n  Lines match: {len(d_lines) == len(b_lines)}")
    print()
    return len(d_done) > 0 and len(b_done) > 0


def test_cancel_stream() -> bool:
    print("=" * 60)
    print("[TEST] Stream cancellation (client abort)")

    r = requests.post(
        f"{BALANCER}/api/generate",
        json={"model": MODEL,
              "prompt": "Напиши очень длинный текст из 1000 слов про историю Рима.",
              "stream": True},
        stream=True, timeout=ARGS.timeout,
    )

    chunks = 0
    for line in r.iter_lines(decode_unicode=True):
        if line:
            chunks += 1
            if chunks >= 5:
                print(f"  Aborting after {chunks} chunks...")
                r.close()
                break

    print(f"  Cancelled after {chunks} chunks - connection closed")
    print(f"  (Check balancer logs for cleanup)")
    print()
    return True


def test_timeout_handling() -> None:
    print("=" * 60)
    print("[TEST] Timeout handling")

    try:
        r = requests.post(
            f"{BALANCER}/api/generate",
            json={"model": MODEL, "prompt": "Привет", "stream": True},
            stream=True, timeout=1,  # Очень короткий таймаут
        )
        for line in r.iter_lines(decode_unicode=True):
            if line:
                print(f"  Got chunk: {line[:80]}")
    except requests.exceptions.Timeout:
        print("  Got expected timeout exception")
    except Exception as e:
        print(f"  Got exception: {type(e).__name__}: {e}")
    print()


def test_non_streaming_large() -> None:
    print("=" * 60)
    print("[TEST] Non-streaming large response")
    try:
        r = requests.post(
            f"{BALANCER}/api/generate",
            json={"model": MODEL, "prompt": "Расскажи анекдот.", "stream": False},
            timeout=60,
        )
        print(f"Status: {r.status_code}, CT: {r.headers.get('Content-Type','')}")
        data = r.json()
        resp = data.get("response", "")
        done = data.get("done", False)
        eval_count = data.get("eval_count", 0)
        print(f"Response: {len(resp)} chars, done: {done}, eval_count: {eval_count}")
        print(f"Text: {resp[:200]}")
    except Exception as e:
        print(f"ERROR: {e}")
    print()


def test_v1_models() -> None:
    print("=" * 60)
    print("[TEST] /v1/models (OpenAI compatibility)")

    r = requests.get(f"{CPPWORKER}/v1/models", timeout=10)
    print(f"DIRECT: Status={r.status_code}")
    if r.status_code == 200:
        data = r.json()
        models = data.get("data", data.get("models", []))
        print(f"  Models: {len(models)}")
        for m in models[:3]:
            print(f"    - {m.get('id', m.get('name', '?'))}")

    r2 = requests.get(f"{BALANCER}/v1/models", timeout=10)
    print(f"BALANCER: Status={r2.status_code}")
    if r2.status_code == 200:
        data2 = r2.json()
        models2 = data2.get("data", data2.get("models", []))
        print(f"  Models: {len(models2)}")
    print()


def test_api_tags() -> None:
    print("=" * 60)
    print("[TEST] /api/tags aggregation")

    r = requests.get(f"{CPPWORKER}/api/tags", timeout=10)
    print(f"DIRECT: Status={r.status_code}")
    if r.status_code == 200:
        data = r.json()
        tags = data.get("models", data.get("tags", []))
        print(f"  Models: {len(tags)}")

    r2 = requests.get(f"{BALANCER}/api/tags", timeout=10)
    print(f"BALANCER: Status={r2.status_code}")
    if r2.status_code == 200:
        data2 = r2.json()
        tags2 = data2.get("models", data2.get("tags", []))
        print(f"  Models: {len(tags2)}")
    print()


def main() -> int:
    global ARGS, BALANCER, CPPWORKER, MODEL
    ARGS = parse_args()
    BALANCER = ARGS.balancer.rstrip("/")
    CPPWORKER = ARGS.cppworker.rstrip("/")
    MODEL = ARGS.model

    print("=" * 60)
    print("DEEP STREAMING TESTS")
    print(f"Balancer={BALANCER}  CppWorker={CPPWORKER}  Model={MODEL}")
    print("=" * 60)
    print()

    results = {}

    results["large_response"] = test_large_response()
    time.sleep(1)

    results["format_comparison"] = test_format_comparison()
    time.sleep(1)

    results["sse_format_comparison"] = test_sse_format_comparison()
    time.sleep(1)

    test_cancel_stream()
    time.sleep(2)

    test_timeout_handling()
    time.sleep(1)

    test_non_streaming_large()
    time.sleep(1)

    test_v1_models()
    time.sleep(1)

    test_api_tags()

    print("=" * 60)
    print("DEEP TEST SUMMARY")
    print("=" * 60)

    passed = sum(1 for v in results.values() if v)
    total = len(results)

    for name, result in results.items():
        status = "PASS" if result else "FAIL"
        print(f"  [{status}] {name}")

    print(f"\n  {passed}/{total} passed")
    return 0 if passed == total else 1


if __name__ == "__main__":
    sys.exit(main())