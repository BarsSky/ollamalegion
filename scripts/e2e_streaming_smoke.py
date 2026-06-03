#!/usr/bin/env python3
"""
Streaming smoke-тест OllamaLegion API:
/api/generate, /api/chat, /v1/chat/completions, /v1/completions,
non-streaming и endpoint discovery. Тестирует потоковую работу:
клиент → балансер → cppworker.

Примеры:
    python scripts/e2e_streaming_smoke.py
    python scripts/e2e_streaming_smoke.py --balancer http://localhost:18080 \\
        --admin http://localhost:18081 --cppworker http://localhost:18092
"""
import argparse
import json
import os
import sys
import time

import requests


# ============================================================
# Аргументы / ENV
# ============================================================

def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="Streaming smoke-тест OllamaLegion API")
    p.add_argument(
        "--balancer",
        default=os.environ.get("OLLAMALEGION_BALANCER", "http://localhost:18080"),
        help="URL балансировщика (env: OLLAMALEGION_BALANCER)",
    )
    p.add_argument(
        "--admin",
        default=os.environ.get("OLLAMALEGION_ADMIN", "http://localhost:18081"),
        help="URL Management API балансировщика (env: OLLAMALEGION_ADMIN)",
    )
    p.add_argument(
        "--cppworker",
        default=os.environ.get("OLLAMALEGION_CPPWORKER", "http://localhost:18092"),
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
        help="Таймаут запросов (env: OLLAMALEGION_TIMEOUT)",
    )
    p.add_argument(
        "--skip-add-backend",
        action="store_true",
        help="Не пытаться регистрировать тестовый cppworker-stub",
    )
    return p.parse_args()


ARGS = None
BALANCER = ""
ADMIN = ""
CPPWORKER_DIRECT = ""
MODEL = ""


def add_backend():
    """Добавляем cppworker-stub как бэкенд"""
    payload = {
        "id": "cppworker-stub",
        "name": "CppWorker Stub",
        "host": "172.28.0.2",
        "ollamaPort": 18092,
        "type": "llama_cpp",
        "weight": 10,
        "maxConcurrentRequests": 4,
    }
    r = requests.post(f"{ADMIN}/api/v1/backends", json=payload)
    print(f"[ADD BACKEND] Status: {r.status_code}")
    print(f"[ADD BACKEND] Response: {r.text[:500]}")
    return r.status_code in (200, 201)


def check_health():
    """Проверка здоровья"""
    r = requests.get(f"{ADMIN}/api/v1/health")
    print(f"[HEALTH] Status: {r.status_code}")
    try:
        print(f"[HEALTH] Response: {json.dumps(r.json(), indent=2, ensure_ascii=False)[:500]}")
    except Exception:
        print(f"[HEALTH] Response: {r.text[:500]}")


def check_backends():
    """Проверка бэкендов"""
    r = requests.get(f"{ADMIN}/api/v1/backends")
    data = r.json()
    print(f"[BACKENDS] Total: {data.get('total', 0)}")
    for b in data.get("backends", []):
        print(f"  - {b['id']}: status={b['status']}, type={b.get('type', 'N/A')}")


def test_ollama_generate_stream(use_balancer: bool = True) -> bool:
    """Тест /api/generate с streaming (NDJSON)"""
    url = f"{BALANCER}/api/generate" if use_balancer else f"{CPPWORKER_DIRECT}/api/generate"
    payload = {
        "model": MODEL,
        "prompt": "Напиши короткое стихотворение из 4 строк про весну.",
        "stream": True,
    }
    print(f"\n{'='*60}")
    print(f"[TEST] /api/generate (NDJSON stream) → {'BALANCER' if use_balancer else 'CPPWORKER_DIRECT'}")
    print(f"URL: {url}")

    try:
        r = requests.post(url, json=payload, stream=True, timeout=ARGS.timeout)
        print(f"Status: {r.status_code}")
        print(f"Content-Type: {r.headers.get('Content-Type', 'N/A')}")

        content_type = r.headers.get("Content-Type", "")
        if "x-ndjson" not in content_type:
            print(f"⚠️  WARNING: Expected application/x-ndjson, got {content_type}")

        chunks = []
        full_response = ""
        done_chunk = None

        for line in r.iter_lines(decode_unicode=True):
            if line:
                chunks.append(line)
                try:
                    data = json.loads(line)
                    if data.get("done") is True:
                        done_chunk = data
                        print(f"  ✓ DONE chunk: {json.dumps(data, ensure_ascii=False)[:200]}")
                    elif data.get("response"):
                        full_response += data["response"]
                except json.JSONDecodeError:
                    print(f"  ⚠️  Non-JSON line: {line[:100]}")

        print(f"\n  Full response ({len(full_response)} chars):")
        print(f"  {full_response[:300]}...")
        print(f"  Total chunks: {len(chunks)}")

        if not done_chunk:
            print(f"  🔴 FAIL: No 'done:true' chunk received!")
            return False
        if not full_response:
            print(f"  🔴 FAIL: No response tokens received!")
            return False

        print(f"  ✅ PASS")
        return True
    except Exception as e:
        print(f"  🔴 ERROR: {e}")
        return False


def test_ollama_chat_stream(use_balancer: bool = True) -> bool:
    """Тест /api/chat с streaming (NDJSON)"""
    url = f"{BALANCER}/api/chat" if use_balancer else f"{CPPWORKER_DIRECT}/api/chat"
    payload = {
        "model": MODEL,
        "messages": [{"role": "user", "content": "Привет! Расскажи кратко о себе в 2 предложениях."}],
        "stream": True,
    }
    print(f"\n{'='*60}")
    print(f"[TEST] /api/chat (NDJSON stream) → {'BALANCER' if use_balancer else 'CPPWORKER_DIRECT'}")
    print(f"URL: {url}")

    try:
        r = requests.post(url, json=payload, stream=True, timeout=ARGS.timeout)
        print(f"Status: {r.status_code}")
        print(f"Content-Type: {r.headers.get('Content-Type', 'N/A')}")

        chunks = []
        full_response = ""
        done_chunk = None

        for line in r.iter_lines(decode_unicode=True):
            if line:
                chunks.append(line)
                try:
                    data = json.loads(line)
                    if data.get("done") is True:
                        done_chunk = data
                        print(f"  ✓ DONE chunk: {json.dumps(data, ensure_ascii=False)[:200]}")
                    elif data.get("message", {}).get("content"):
                        full_response += data["message"]["content"]
                except json.JSONDecodeError:
                    print(f"  ⚠️  Non-JSON line: {line[:100]}")

        print(f"\n  Full response ({len(full_response)} chars):")
        print(f"  {full_response[:300]}...")
        print(f"  Total chunks: {len(chunks)}")

        if not done_chunk:
            print(f"  🔴 FAIL: No 'done:true' chunk received!")
            return False

        print(f"  ✅ PASS")
        return True
    except Exception as e:
        print(f"  🔴 ERROR: {e}")
        return False


def test_openai_chat_completions_stream(use_balancer: bool = True) -> bool:
    """Тест /v1/chat/completions с streaming (SSE)"""
    url = f"{BALANCER}/v1/chat/completions" if use_balancer else f"{CPPWORKER_DIRECT}/v1/chat/completions"
    payload = {
        "model": MODEL,
        "messages": [{"role": "user", "content": "Напиши короткое хайку про закат."}],
        "stream": True,
    }
    print(f"\n{'='*60}")
    print(f"[TEST] /v1/chat/completions (SSE stream) → {'BALANCER' if use_balancer else 'CPPWORKER_DIRECT'}")
    print(f"URL: {url}")

    try:
        r = requests.post(url, json=payload, stream=True, timeout=ARGS.timeout)
        print(f"Status: {r.status_code}")
        print(f"Content-Type: {r.headers.get('Content-Type', 'N/A')}")

        content_type = r.headers.get("Content-Type", "")
        if "event-stream" not in content_type:
            print(f"⚠️  WARNING: Expected text/event-stream, got {content_type}")

        chunks = []
        full_response = ""
        got_done = False

        for line in r.iter_lines(decode_unicode=True):
            if line:
                chunks.append(line)
                if line.startswith("data: "):
                    data_str = line[6:]
                    if data_str == "[DONE]":
                        got_done = True
                        print(f"  ✓ [DONE] received")
                    else:
                        try:
                            data = json.loads(data_str)
                            choices = data.get("choices", [])
                            if choices and choices[0].get("delta", {}).get("content"):
                                full_response += choices[0]["delta"]["content"]
                            finish_reason = choices[0].get("finish_reason") if choices else None
                            if finish_reason:
                                print(f"  ✓ finish_reason: {finish_reason}")
                        except json.JSONDecodeError:
                            print(f"  ⚠️  Non-JSON data line: {data_str[:100]}")

        print(f"\n  Full response ({len(full_response)} chars):")
        print(f"  {full_response[:300]}...")
        print(f"  Total SSE lines: {len(chunks)}")

        if not got_done:
            print(f"  🔴 FAIL: No 'data: [DONE]' received!")
            return False
        if not full_response:
            print(f"  🔴 FAIL: No response content received!")
            return False

        print(f"  ✅ PASS")
        return True
    except Exception as e:
        print(f"  🔴 ERROR: {e}")
        return False


def test_openai_completions_stream(use_balancer: bool = True) -> bool:
    """Тест /v1/completions с streaming (SSE)"""
    url = f"{BALANCER}/v1/completions" if use_balancer else f"{CPPWORKER_DIRECT}/v1/completions"
    payload = {
        "model": MODEL,
        "prompt": "Продолжи фразу: Однажды в студёную зимнюю пору",
        "stream": True,
        "max_tokens": 50,
    }
    print(f"\n{'='*60}")
    print(f"[TEST] /v1/completions (SSE stream) → {'BALANCER' if use_balancer else 'CPPWORKER_DIRECT'}")
    print(f"URL: {url}")

    try:
        r = requests.post(url, json=payload, stream=True, timeout=ARGS.timeout)
        print(f"Status: {r.status_code}")
        print(f"Content-Type: {r.headers.get('Content-Type', 'N/A')}")

        chunks = []
        full_response = ""
        got_done = False

        for line in r.iter_lines(decode_unicode=True):
            if line:
                chunks.append(line)
                if line.startswith("data: "):
                    data_str = line[6:]
                    if data_str == "[DONE]":
                        got_done = True
                        print(f"  ✓ [DONE] received")
                    else:
                        try:
                            data = json.loads(data_str)
                            choices = data.get("choices", [])
                            if choices and choices[0].get("text"):
                                full_response += choices[0]["text"]
                            finish_reason = choices[0].get("finish_reason") if choices else None
                            if finish_reason:
                                print(f"  ✓ finish_reason: {finish_reason}")
                        except json.JSONDecodeError:
                            print(f"  ⚠️  Non-JSON data line: {data_str[:100]}")

        print(f"\n  Full response ({len(full_response)} chars):")
        print(f"  {full_response[:300]}...")
        print(f"  Total SSE lines: {len(chunks)}")

        if not got_done:
            print(f"  🔴 FAIL: No 'data: [DONE]' received!")
            return False

        print(f"  ✅ PASS")
        return True
    except Exception as e:
        print(f"  🔴 ERROR: {e}")
        return False


def test_non_streaming(use_balancer: bool = True) -> bool:
    """Тест non-streaming /api/generate"""
    url = f"{BALANCER}/api/generate" if use_balancer else f"{CPPWORKER_DIRECT}/api/generate"
    payload = {
        "model": MODEL,
        "prompt": "Скажи 'привет'.",
        "stream": False,
    }
    print(f"\n{'='*60}")
    print(f"[TEST] /api/generate (non-streaming) → {'BALANCER' if use_balancer else 'CPPWORKER_DIRECT'}")
    print(f"URL: {url}")

    try:
        r = requests.post(url, json=payload, timeout=60)
        print(f"Status: {r.status_code}")
        print(f"Content-Type: {r.headers.get('Content-Type', 'N/A')}")

        data = r.json()
        response_text = data.get("response", "")
        done = data.get("done", False)

        print(f"  Response: {response_text[:200]}")
        print(f"  Done: {done}")

        if not done:
            print(f"  🔴 FAIL: 'done' is not True!")
            return False
        if not response_text:
            print(f"  🔴 FAIL: No response text!")
            return False

        print(f"  ✅ PASS")
        return True
    except Exception as e:
        print(f"  🔴 ERROR: {e}")
        return False


def test_direct_cppworker_endpoints():
    """Проверяем, какие endpoints есть у cppworker напрямую"""
    print(f"\n{'='*60}")
    print(f"[DISCOVERY] Testing cppworker endpoints directly")

    endpoints = [
        ("GET", "/health"),
        ("GET", "/v1/models"),
        ("GET", "/api/tags"),
        ("POST", "/api/generate"),
        ("POST", "/api/chat"),
        ("POST", "/v1/chat/completions"),
        ("POST", "/v1/completions"),
        ("POST", "/v1/embeddings"),
    ]

    available = []
    missing = []

    for method, path in endpoints:
        url = f"{CPPWORKER_DIRECT}{path}"
        try:
            if method == "GET":
                r = requests.get(url, timeout=5)
            else:
                r = requests.post(url, json={"model": "test"}, timeout=5)

            status = r.status_code
            if status != 404:
                available.append((method, path, status))
                print(f"  ✅ {method} {path} → {status}")
            else:
                missing.append((method, path))
                print(f"  ❌ {method} {path} → {status} (NOT FOUND)")
        except Exception as e:
            missing.append((method, path))
            print(f"  ❌ {method} {path} → ERROR: {e}")

    print(f"\n  Available: {len(available)}, Missing: {len(missing)}")
    return available, missing


def main() -> int:
    global ARGS, BALANCER, ADMIN, CPPWORKER_DIRECT, MODEL
    ARGS = parse_args()
    BALANCER = ARGS.balancer.rstrip("/")
    ADMIN = ARGS.admin.rstrip("/")
    CPPWORKER_DIRECT = ARGS.cppworker.rstrip("/")
    MODEL = ARGS.model

    print("=" * 60)
    print("OLLAMALEGION API STREAMING TEST SUITE")
    print(f"Balancer={BALANCER}  Admin={ADMIN}  CppWorker={CPPWORKER_DIRECT}  Model={MODEL}")
    print("=" * 60)

    print("\n--- PHASE 1: Health Check ---")
    check_health()

    if not ARGS.skip_add_backend:
        print("\n--- PHASE 2: Register Backend ---")
        if not add_backend():
            print("⚠️  Failed to add backend, continuing anyway...")
        time.sleep(2)
        check_backends()
    else:
        print("\n--- PHASE 2: Register Backend --- (skipped)")

    print("\n--- PHASE 3: Endpoint Discovery ---")
    test_direct_cppworker_endpoints()

    print("\n--- PHASE 4: Streaming Tests via Balancer ---")
    results = {}

    results["generate_stream"] = test_ollama_generate_stream(use_balancer=True)
    time.sleep(2)

    results["chat_stream"] = test_ollama_chat_stream(use_balancer=True)
    time.sleep(2)

    results["openai_chat_stream"] = test_openai_chat_completions_stream(use_balancer=True)
    time.sleep(2)

    results["openai_completions_stream"] = test_openai_completions_stream(use_balancer=True)
    time.sleep(2)

    results["non_streaming"] = test_non_streaming(use_balancer=True)

    print("\n\n" + "=" * 60)
    print("RESULTS SUMMARY")
    print("=" * 60)

    passed = sum(1 for v in results.values() if v)
    total = len(results)

    for test_name, result in results.items():
        status = "✅ PASS" if result else "🔴 FAIL"
        print(f"  {status} — {test_name}")

    print(f"\n  Total: {passed}/{total} passed")

    if passed < total:
        print("\n🔴 SOME TESTS FAILED — review the output above for details")
    else:
        print("\n✅ ALL TESTS PASSED")

    return 0 if passed == total else 1


if __name__ == "__main__":
    sys.exit(main())