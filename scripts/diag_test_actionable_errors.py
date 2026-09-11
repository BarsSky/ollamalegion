#!/usr/bin/env python3
"""Test R60.40 actionable diagnostics на deployed bundle.

Проверяет что:
1. 503 (auto-load in progress) содержит feasible_max_context / suggestion
2. Retry-After header присутствует и разумный
3. JSON содержит target_n_ctx / suggestion / backend_id
"""
import requests
import json
import sys

BALANCER = "http://localhost:18080"
MODEL = "Qwen3-Instruct-2507-q4km"


def test_actionable_503():
    """Request that triggers auto-load error path."""
    # Body with num_ctx much bigger than current loaded — forces preflight reload
    body = {
        "model": MODEL,
        "messages": [{"role": "user", "content": "Привет"}],
        "stream": False,
        "options": {"num_ctx": 32768, "num_predict": 1024, "temperature": 0.7},
    }
    print("\n=== Test 1: trigger auto-load 503 with high num_ctx ===")
    try:
        r = requests.post(f"{BALANCER}/api/chat", json=body, timeout=120)
        print(f"HTTP {r.status_code}")
        print(f"Content-Type: {r.headers.get('Content-Type')}")
        print(f"Retry-After header: {r.headers.get('Retry-After')}")
        try:
            data = r.json()
            print(f"\nResponse JSON keys: {list(data.keys())}")
            print(json.dumps(data, indent=2, ensure_ascii=False)[:1500])
            # Check for actionable fields
            actionable_fields = ["model", "target_n_ctx", "feasible_max_context",
                                 "gguf_max_context", "retry_after", "suggestion"]
            present = [f for f in actionable_fields if f in data]
            missing = [f for f in actionable_fields if f not in data]
            if present:
                print(f"\n✅ actionable fields present: {present}")
            if missing:
                print(f"❌ actionable fields MISSING: {missing}")
            return data
        except json.JSONDecodeError:
            print(f"Not JSON: {r.text[:300]}")
            return None
    except requests.exceptions.Timeout:
        print(f"Timeout after 120s")
        return None


def test_real_inference_succeeds():
    """After auto-load, normal request should succeed."""
    body = {
        "model": MODEL,
        "messages": [{"role": "user", "content": "Скажи привет за 1 предложение"}],
        "stream": False,
        "options": {"num_ctx": 4096, "num_predict": 32, "temperature": 0},
    }
    print("\n=== Test 2: real inference after auto-load (n_ctx=4096) ===")
    try:
        r = requests.post(f"{BALANCER}/api/chat", json=body, timeout=60)
        print(f"HTTP {r.status_code}, Retry-After: {r.headers.get('Retry-After')}")
        try:
            data = r.json()
            msg = data.get("message", {})
            content = msg.get("content", "") if isinstance(msg, dict) else ""
            done = data.get("done")
            done_reason = data.get("done_reason")
            eval_count = data.get("eval_count", 0)
            print(f"  done: {done}, done_reason: {done_reason}, eval_count: {eval_count}")
            print(f"  content_len: {len(content)}")
            print(f"  content[:100]: {content[:100]}")
            return data
        except json.JSONDecodeError:
            print(f"Not JSON: {r.text[:300]}")
            return None
    except requests.exceptions.Timeout:
        print(f"Timeout after 60s")
        return None


def main() -> int:
    print("=" * 60)
    print(f"R60.40 actionable diagnostics test")
    print(f"balancer: {BALANCER}, model: {MODEL}")
    print("=" * 60)
    r1 = test_actionable_503()
    r2 = test_real_inference_succeeds()
    print("\n" + "=" * 60)
    if r1:
        has_suggestion = "suggestion" in r1 and r1["suggestion"]
        has_feasible = "feasible_max_context" in r1 and r1["feasible_max_context"] > 0
        has_gguf = "gguf_max_context" in r1 and r1["gguf_max_context"] > 0
        print(f"503 has suggestion: {'✅' if has_suggestion else '❌'}")
        print(f"503 has feasible_max_context: {'✅' if has_feasible else '❌'}")
        print(f"503 has gguf_max_context: {'✅' if has_gguf else '❌'}")
    if r2:
        success = r2.get("done") and r2.get("message", {}).get("content")
        print(f"Normal inference succeeded: {'✅' if success else '❌'}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
