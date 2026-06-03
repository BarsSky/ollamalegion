#!/usr/bin/env python3
"""
Диагностический streaming-тест OllamaLegion: streaming/non-streaming,
через балансер и напрямую к cppworker, с проверкой `done:true`.

Примеры:
    pip install httpx requests
    python scripts/e2e_streaming_diag.py
    python scripts/e2e_streaming_diag.py --balancer http://localhost:18080 \\
        --cppworker http://localhost:18092 --model llama3.1:8b
"""
import argparse
import os
import sys

try:
    import httpx
except ImportError:
    print("ERROR: требуется пакет httpx. Установите: pip install httpx", file=sys.stderr)
    sys.exit(2)

import requests


# ============================================================
# Аргументы / ENV
# ============================================================

def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="Диагностический streaming-тест OllamaLegion")
    p.add_argument(
        "--balancer",
        default=os.environ.get("OLLAMALEGION_BALANCER", "http://localhost:18080"),
        help="URL балансировщика (env: OLLAMALEGION_BALANCER)",
    )
    p.add_argument(
        "--cppworker",
        default=os.environ.get("OLLAMALEGION_CPPWORKER", "http://localhost:18092"),
        help="URL CppWorker напрямую (env: OLLAMALEGION_CPPWORKER)",
    )
    p.add_argument(
        "--model",
        default=os.environ.get("OLLAMALEGION_MODEL", "gemma-4-E4B-it"),
        help="Имя тестовой модели (env: OLLAMALEGION_MODEL)",
    )
    p.add_argument(
        "--timeout",
        type=float,
        default=float(os.environ.get("OLLAMALEGION_TIMEOUT", "120")),
        help="Таймаут запросов (env: OLLAMALEGION_TIMEOUT)",
    )
    return p.parse_args()


BASE_URL = ""
CPP_URL = ""
MODEL = ""
TIMEOUT = 120.0


def test_streaming(endpoint, body, label):
    print(f"\n{'='*60}")
    print(f"{label}")
    print(f"{'='*60}")

    try:
        with httpx.stream("POST", f"{BASE_URL}{endpoint}", json=body, timeout=TIMEOUT) as response:
            print(f"Status: {response.status_code}")
            print(f"Content-Type: {response.headers.get('content-type')}")
            chunks = 0
            last_chunk = None
            for line in response.iter_lines():
                chunks += 1
                last_chunk = line
                if chunks <= 5 or "done" in (line or "").lower():
                    print(f"  [{chunks}] {(line or '')[:200]}")
            print(f"Total chunks received: {chunks}")
            print(f"Last chunk: {last_chunk}")
            if last_chunk and '"done":true' in last_chunk:
                print("✅ SUCCESS: Got final done:true")
                return True
            else:
                print("❌ FAIL: Missing done:true in final chunk!")
                return False
    except Exception as e:
        print(f"❌ ERROR: {e}")
        return False


def test_non_streaming(endpoint, body, label):
    print(f"\n{'='*60}")
    print(f"{label}")
    print(f"{'='*60}")

    try:
        response = httpx.post(f"{BASE_URL}{endpoint}", json=body, timeout=TIMEOUT)
        print(f"Status: {response.status_code}")
        data = response.json()
        print(f"Response keys: {list(data.keys())}")
        print(f"Done: {data.get('done')}")
        response_text = data.get("response", "")
        print(f"Response preview: {response_text[:300]}")
        if data.get("done"):
            print("✅ SUCCESS")
            return True
        else:
            print("❌ FAIL: done != true")
            return False
    except Exception as e:
        print(f"❌ ERROR: {e}")
        return False


def test_direct_streaming(endpoint, body, label, base_url):
    """Streaming напрямую к указанному base_url (без балансера)."""
    print(f"\n{'='*60}")
    print(f"{label}")
    print(f"{'='*60}")

    try:
        with httpx.stream("POST", f"{base_url}{endpoint}", json=body, timeout=TIMEOUT) as response:
            print(f"Status: {response.status_code}")
            chunks = 0
            last_chunk = None
            for line in response.iter_lines():
                chunks += 1
                last_chunk = line
                if chunks <= 5 or "done" in (line or "").lower():
                    print(f"  [{chunks}] {(line or '')[:200]}")
            print(f"Total chunks: {chunks}")
            print(f"Last: {last_chunk}")
            if last_chunk and '"done":true' in last_chunk:
                print("✅ SUCCESS")
                return True
            else:
                print("❌ FAIL")
                return False
    except Exception as e:
        print(f"❌ ERROR: {e}")
        return False


def main() -> int:
    global BASE_URL, CPP_URL, MODEL, TIMEOUT
    ARGS = parse_args()
    BASE_URL = ARGS.balancer.rstrip("/")
    CPP_URL = ARGS.cppworker.rstrip("/")
    MODEL = ARGS.model
    TIMEOUT = ARGS.timeout

    print("=" * 60)
    print("OLLAMALEGION STREAMING DIAGNOSTIC")
    print(f"Balancer={BASE_URL}  CppWorker={CPP_URL}  Model={MODEL}")
    print("=" * 60)

    # Проверяем, что эндпоинт cppworker вообще доступен
    print("\n[PRECHECK] CppWorker /health")
    try:
        r = requests.get(f"{CPP_URL}/health", timeout=5)
        print(f"  status={r.status_code}, body={r.text[:200]}")
    except Exception as e:
        print(f"  ERROR: {e}")

    results = {}

    # Test 1: Streaming /api/generate via balancer
    results["t1_bal_generate_stream"] = test_streaming(
        "/api/generate",
        {"model": MODEL, "prompt": "2+2=4, верно? Ответь одним словом.", "stream": True},
        "Test 1: Streaming /api/generate via balancer",
    )

    # Test 2: Non-streaming /api/generate via balancer
    results["t2_bal_generate_nostream"] = test_non_streaming(
        "/api/generate",
        {"model": MODEL, "prompt": "Скажи привет!", "stream": False},
        "Test 2: Non-streaming /api/generate via balancer",
    )

    # Test 3: Streaming /api/generate directly to cppworker
    print("\nTest 3: Streaming /api/generate directly to cppworker")
    results["t3_cpp_generate_stream"] = test_direct_streaming(
        "/api/generate",
        {"model": MODEL, "prompt": "2+2=?", "stream": True},
        f"Test 3: Streaming /api/generate directly ({CPP_URL})",
        CPP_URL,
    )

    # Test 4: /api/chat streaming via balancer
    results["t4_bal_chat_stream"] = test_streaming(
        "/api/chat",
        {"model": MODEL, "messages": [{"role": "user", "content": "Привет!"}], "stream": True},
        "Test 4: Streaming /api/chat via balancer",
    )

    # Test 5: /api/chat non-streaming via balancer
    results["t5_bal_chat_nostream"] = test_non_streaming(
        "/api/chat",
        {"model": MODEL, "messages": [{"role": "user", "content": "Привет!"}], "stream": False},
        "Test 5: Non-streaming /api/chat via balancer",
    )

    print("\n" + "=" * 60)
    print("SUMMARY")
    print("=" * 60)
    passed = sum(1 for v in results.values() if v)
    total = len(results)
    for name, ok in results.items():
        print(f"  [{'PASS' if ok else 'FAIL'}] {name}")
    print(f"\n  {passed}/{total} passed")
    return 0 if passed == total else 1


if __name__ == "__main__":
    sys.exit(main())