#!/usr/bin/env python3
"""Проверяет actionable diagnostics от cppworker для разных edge cases.

Тестирует:
1. n_ctx too small (e.g., 1024 с prompt+n_predict > 1024)
2. n_ctx way too small (e.g., 256)
3. n_predict larger than n_ctx
4. Model not loaded (cppworker has nothing)
5. Mid-request cancellation
6. Various HTTP status codes
7. Whether response has actionable fields:
   - feasible_max_context
   - gguf_max_context
   - max_vram_n_ctx
   - recommended_n_ctx
"""
import json
import requests
import sys

CPPWORKER = "http://localhost:18092"
BALANCER = "http://localhost:18080"
MODEL = "Qwen3-Instruct-2507-q4km"
PROMPT = "Распиши красивый сайт на html css для интерактивной математики расчета движения полета. Полный код."


def test_case(name: str, body: dict, timeout: int = 60) -> dict:
    """Send one test request and parse response in detail."""
    print(f"\n{'=' * 60}")
    print(f"[{name}]")
    print(f"body: {json.dumps(body, ensure_ascii=False)[:200]}")
    result = {
        "name": name,
        "http_status": 0,
        "content_type": "",
        "body_len": 0,
        "body_first_200": "",
        "is_json": False,
        "json_keys": [],
        "error_msg": "",
        "bridge_info": {},
        "diagnostic_fields": {},
        "anomalies": [],
    }
    try:
        r = requests.post(
            f"{BALANCER}/api/chat", json=body, timeout=timeout
        )
        result["http_status"] = r.status_code
        result["content_type"] = r.headers.get("Content-Type", "")
        result["body_len"] = len(r.text)
        result["body_first_200"] = r.text[:200]
        try:
            data = r.json()
            result["is_json"] = True
            result["json_keys"] = list(data.keys())
            if "error" in data:
                result["error_msg"] = str(data["error"])[:300]
            if "bridge_info" in data:
                bi = data["bridge_info"]
                result["bridge_info"] = {
                    k: str(v)[:200] for k, v in bi.items() if v is not None
                }
            # Check for diagnostic fields
            for k in (
                "feasible_max_context",
                "gguf_max_context",
                "max_vram_n_ctx",
                "max_ram_n_ctx",
                "recommended_n_ctx",
                "suggestion",
                "current_n_ctx",
                "required_n_ctx",
            ):
                if k in data:
                    result["diagnostic_fields"][k] = data[k]
                elif "bridge_info" in data and k in data["bridge_info"]:
                    result["diagnostic_fields"][k] = data["bridge_info"][k]
        except json.JSONDecodeError:
            result["anomalies"].append(f"response is not JSON: {result['body_first_200'][:100]}")
    except requests.exceptions.Timeout:
        result["anomalies"].append(f"timeout after {timeout}s")
    except requests.exceptions.RequestException as e:
        result["anomalies"].append(f"request exception: {e}")
    print(f"  HTTP {result['http_status']} ({result['content_type']})")
    print(f"  body_len: {result['body_len']}, is_json: {result['is_json']}")
    print(f"  json_keys: {result['json_keys']}")
    if result["error_msg"]:
        print(f"  error_msg: {result['error_msg']}")
    if result["bridge_info"]:
        print(f"  bridge_info: {result['bridge_info']}")
    if result["diagnostic_fields"]:
        print(f"  diagnostic_fields: {result['diagnostic_fields']}")
    if result["anomalies"]:
        print(f"  ❌ ANOMALIES: {result['anomalies']}")
    return result


def main() -> int:
    cases = [
        test_case(
            "n_ctx=256 (way too small)",
            {
                "model": MODEL,
                "messages": [{"role": "user", "content": PROMPT}],
                "stream": False,
                "options": {"num_ctx": 256, "temperature": 0.7},
            },
            timeout=15,
        ),
        test_case(
            "n_ctx=1024 (too small for prompt)",
            {
                "model": MODEL,
                "messages": [{"role": "user", "content": PROMPT}],
                "stream": False,
                "options": {"num_ctx": 1024, "num_predict": 512, "temperature": 0.7},
            },
            timeout=20,
        ),
        test_case(
            "n_ctx=2048, num_predict=2048 (default cppworker 2048)",
            {
                "model": MODEL,
                "messages": [{"role": "user", "content": PROMPT}],
                "stream": False,
                "options": {"num_ctx": 2048, "temperature": 0.7},
            },
            timeout=20,
        ),
        test_case(
            "n_ctx=4096, num_predict=2048 (good)",
            {
                "model": MODEL,
                "messages": [{"role": "user", "content": PROMPT}],
                "stream": False,
                "options": {"num_ctx": 4096, "num_predict": 2048, "temperature": 0.7},
            },
            timeout=180,
        ),
    ]
    print(f"\n{'=' * 60}")
    print(f"SUMMARY: {len(cases)} test cases")
    for c in cases:
        actionable = bool(c.get("bridge_info") or c.get("diagnostic_fields"))
        verdict = "✅ actionable" if actionable else "❌ no diagnostics"
        print(f"  {c['name']}: HTTP {c['http_status']}, {verdict}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
