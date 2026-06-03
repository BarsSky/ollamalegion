#!/usr/bin/env python3
"""
Полный coverage-отчёт по API OllamaLegion: все основные endpoint'ы
через балансер и напрямую + отдельная проверка ещё-не-реализованных
Ollama endpoint'ов (missing endpoints).

Примеры:
    python scripts/e2e_api_coverage.py
    python scripts/e2e_api_coverage.py --balancer http://localhost:18080 \\
        --cppworker http://localhost:18093 --model llama3.1:8b
"""
import argparse
import os
import sys

import requests


# ============================================================
# Аргументы / ENV
# ============================================================

def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="Coverage-отчёт API OllamaLegion")
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
        default=int(os.environ.get("OLLAMALEGION_TIMEOUT", "30")),
        help="Таймаут запросов (env: OLLAMALEGION_TIMEOUT)",
    )
    return p.parse_args()


ARGS = None
CPP = ""
BAL = ""
MODEL = ""


def test(name: str, method: str, url: str, payload=None,
         timeout: int | None = None, stream: bool = False,
         expected_ct: str | None = None):
    print(f"\n{'='*50}")
    print(f"[{name}] {method} {url}")
    try:
        if method == "GET":
            r = requests.get(url, timeout=timeout or ARGS.timeout)
        else:
            r = requests.post(url, json=payload, timeout=timeout or ARGS.timeout, stream=stream)

        ct = r.headers.get("Content-Type", "")
        status = r.status_code

        print(f"  Status: {status}")
        print(f"  Content-Type: {ct}")

        if expected_ct and expected_ct not in ct:
            print(f"  WARN: Expected {expected_ct}, got {ct}")

        if stream:
            lines = [l for l in r.iter_lines(decode_unicode=True) if l]
            print(f"  Stream lines: {len(lines)}")
            got_done = any("[DONE]" in l for l in lines) or any('"done":true' in l for l in lines)
            print(f"  Got terminal marker: {got_done}")
            if lines:
                print(f"  First: {lines[0][:120]}")
                print(f"  Last:  {lines[-1][:150]}")
            return status, ct, got_done
        else:
            try:
                data = r.json()
                resp_keys = list(data.keys())
                print(f"  Keys: {resp_keys}")
                if "choices" in data:
                    print(f"  Choices: {len(data['choices'])}")
                    c = data["choices"][0]
                    txt = c.get("text") or c.get("message", {}).get("content", "")
                    print(f"  Text: {txt[:150]}")
                    print(f"  Finish: {c.get('finish_reason', 'N/A')}")
                if "response" in data:
                    print(f"  Response: {data['response'][:150]}")
                    print(f"  Done: {data.get('done', False)}")
                return status, ct, data
            except Exception:
                print(f"  Body: {r.text[:200]}")
                return status, ct, None
    except Exception as e:
        print(f"  ERROR: {e}")
        return None, str(e), None


def main() -> int:
    global ARGS, CPP, BAL, MODEL
    ARGS = parse_args()
    CPP = ARGS.cppworker.rstrip("/")
    BAL = ARGS.balancer.rstrip("/")
    MODEL = ARGS.model

    print("=" * 60)
    print("FULL API COVERAGE REPORT")
    print(f"Balancer={BAL}  CppWorker={CPP}  Model={MODEL}")
    print("=" * 60)

    results = []

    # === OLLAMA API (NDJSON streaming) ===
    results.append(("api/generate-stream", *test("api/generate (s)", "POST", f"{BAL}/api/generate",
        {"model": MODEL, "prompt": "Привет", "stream": True},
        stream=True, expected_ct="x-ndjson")))

    results.append(("api/generate-nostream", *test("api/generate (ns)", "POST", f"{BAL}/api/generate",
        {"model": MODEL, "prompt": "Привет", "stream": False})))

    results.append(("api/chat-stream", *test("api/chat (s)", "POST", f"{BAL}/api/chat",
        {"model": MODEL, "messages": [{"role": "user", "content": "Привет"}], "stream": True},
        stream=True, expected_ct="x-ndjson")))

    results.append(("api/chat-nostream", *test("api/chat (ns)", "POST", f"{BAL}/api/chat",
        {"model": MODEL, "messages": [{"role": "user", "content": "Привет"}], "stream": False})))

    # === OPENAI API (SSE streaming) ===
    results.append(("v1/chat/completions-stream", *test("v1/chat/compl (s)", "POST", f"{BAL}/v1/chat/completions",
        {"model": MODEL, "messages": [{"role": "user", "content": "Hi"}], "stream": True},
        stream=True, expected_ct="event-stream")))

    results.append(("v1/chat/completions-nostream", *test("v1/chat/compl (ns)", "POST", f"{BAL}/v1/chat/completions",
        {"model": MODEL, "messages": [{"role": "user", "content": "Hi"}], "stream": False})))

    results.append(("v1/completions-stream", *test("v1/completions (s)", "POST", f"{BAL}/v1/completions",
        {"model": MODEL, "prompt": "Hello", "stream": True, "max_tokens": 30},
        stream=True, expected_ct="event-stream")))

    results.append(("v1/completions-nostream", *test("v1/completions (ns)", "POST", f"{BAL}/v1/completions",
        {"model": MODEL, "prompt": "Hello", "stream": False, "max_tokens": 30})))

    # === LIST/MODELS ===
    results.append(("api/tags-balancer", *test("api/tags (bal)", "GET", f"{BAL}/api/tags")))
    results.append(("v1/models-balancer", *test("v1/models (bal)", "GET", f"{BAL}/v1/models")))
    results.append(("api/tags-direct", *test("api/tags (dir)", "GET", f"{CPP}/api/tags")))
    results.append(("v1/models-direct", *test("v1/models (dir)", "GET", f"{CPP}/v1/models")))

    # === EMBEDDINGS ===
    results.append(("v1/embeddings-direct", *test("v1/embeddings (dir)", "POST", f"{CPP}/v1/embeddings",
        {"model": MODEL, "input": "hello"})))
    results.append(("v1/embeddings-balancer", *test("v1/embeddings (bal)", "POST", f"{BAL}/v1/embeddings",
        {"model": MODEL, "input": "hello"})))
    results.append(("api/embeddings-direct", *test("api/embeddings (dir)", "POST", f"{CPP}/api/embeddings",
        {"model": MODEL, "prompt": "hello"})))

    # === LEGACY ===
    results.append(("api/ollama/generate", *test("api/ollama/gen", "POST", f"{CPP}/api/ollama/generate",
        {"model": MODEL, "prompt": "hi", "stream": True}, stream=True)))

    # === HEALTH ===
    results.append(("health-direct", *test("health (dir)", "GET", f"{CPP}/health")))

    # === MISSING ENDPOINTS (по документации Ollama) ===
    print(f"\n{'='*50}")
    print("[MISSING ENDPOINT CHECK]")

    missing_checks = [
        ("GET",    "/api/version",  "Ollama version"),
        ("GET",    "/api/ps",       "Running models status"),
        ("POST",   "/api/show",     "Model info"),
        ("POST",   "/api/create",   "Create model"),
        ("POST",   "/api/pull",     "Pull model"),
        ("DELETE", "/api/delete",   "Delete model"),
        ("POST",   "/api/copy",     "Copy model"),
    ]

    for method, path, desc in missing_checks:
        url = f"{BAL}{path}"
        try:
            if method == "GET":
                r = requests.get(url, timeout=10)
            elif method == "DELETE":
                r = requests.delete(url, timeout=10)
            else:
                r = requests.post(url, json={"name": "test"}, timeout=10)

            status = r.status_code
            if status == 404:
                print(f"  MISSING: {method} {path} ({desc}) → 404")
            else:
                print(f"  EXISTS:  {method} {path} ({desc}) → {status}")
        except Exception as e:
            print(f"  ERROR:   {method} {path} ({desc}) → {e}")

    # === SUMMARY ===
    print(f"\n{'='*60}")
    print("FULL COVERAGE SUMMARY")
    print(f"{'='*60}")

    failed = []
    passed = []

    for name, status, ct, data in results:
        if status and status < 400:
            passed.append(name)
        else:
            failed.append((name, status, ct))

    print(f"\nPASSED ({len(passed)}):")
    for p in passed:
        print(f"  ✅ {p}")

    if failed:
        print(f"\nFAILED ({len(failed)}):")
        for f, s, c in failed:
            print(f"  ❌ {f}: status={s}, ct={c}")

    return 0 if not failed else 1


if __name__ == "__main__":
    sys.exit(main())