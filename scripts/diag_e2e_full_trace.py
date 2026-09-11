#!/usr/bin/env python3
"""
R60.42 — Full end-to-end trace OpenWebUI → balancer → cppworker.

Reproduces ТОЧНО тот же workflow что делает пользователь:
1. Click Send в OpenWebUI (отправляет POST /api/chat через nginx)
2. Capture FULL request/response с заголовками
3. Trace что происходит на каждом слое (nginx log, balancer log, cppworker log)
4. Показать user-facing UI response
5. Polling пока модель не загрузится ИЛИ не провалится окончательно
"""
import argparse
import json
import os
import subprocess
import sys
import time
from typing import Any
import requests

BALANCER = "http://localhost:18080"
NGINX = "http://localhost:18083"
CPPWORKER = "http://localhost:18092"
MODEL = "Qwen3-Instruct-2507-q4km"

# OpenWebUI-style request (Ollama /api/chat) - точно как шлёт OpenWebUI
PROMPT = (
    "Привет распиши красивый сайт на html css для интерактивной математики "
    "расчета движения полета"
)


def get_cppworker_state() -> dict:
    try:
        r = requests.get(f"{CPPWORKER}/api/models", timeout=5)
        return r.json()
    except Exception as e:
        return {"error": str(e)}


def send_request_via_nginx(body: dict, timeout: int) -> dict:
    """Send request через nginx (как OpenWebUI делает)."""
    print(f"\n>>> Sending via nginx: {NGINX}/api/chat")
    print(f"    body: {json.dumps(body, ensure_ascii=False)[:200]}")
    result = {
        "endpoint": f"{NGINX}/api/chat",
        "elapsed_sec": 0.0,
        "http_status": 0,
        "headers": {},
        "body_text": "",
        "is_html": False,
        "is_json": False,
        "anomalies": [],
    }
    start = time.time()
    try:
        r = requests.post(
            f"{NGINX}/api/chat",
            json=body,
            timeout=timeout,
            headers={"Authorization": "Bearer changeme-bundled-with-agent-token"},
        )
        result["elapsed_sec"] = time.time() - start
        result["http_status"] = r.status_code
        result["headers"] = dict(r.headers)
        result["body_text"] = r.text
        first = r.text[:1] if r.text else ""
        result["is_html"] = first == "<"
        try:
            result["parsed"] = r.json()
            result["is_json"] = True
        except json.JSONDecodeError:
            result["anomalies"].append("body is not JSON")
    except requests.exceptions.Timeout:
        result["elapsed_sec"] = time.time() - start
        result["anomalies"].append(f"timeout after {timeout}s")
    except requests.exceptions.RequestException as e:
        result["elapsed_sec"] = time.time() - start
        result["anomalies"].append(f"request exception: {e}")
    return result


def send_request_via_balancer(body: dict, timeout: int) -> dict:
    """Send request напрямую в balancer (без nginx)."""
    print(f"\n>>> Sending via balancer: {BALANCER}/api/chat")
    result = {
        "endpoint": f"{BALANCER}/api/chat",
        "elapsed_sec": 0.0,
        "http_status": 0,
        "headers": {},
        "body_text": "",
        "is_html": False,
        "is_json": False,
        "anomalies": [],
    }
    start = time.time()
    try:
        r = requests.post(
            f"{BALANCER}/api/chat",
            json=body,
            timeout=timeout,
            headers={"Authorization": "Bearer bundled-default"},
        )
        result["elapsed_sec"] = time.time() - start
        result["http_status"] = r.status_code
        result["headers"] = dict(r.headers)
        result["body_text"] = r.text
        first = r.text[:1] if r.text else ""
        result["is_html"] = first == "<"
        try:
            result["parsed"] = r.json()
            result["is_json"] = True
        except json.JSONDecodeError:
            result["anomalies"].append("body is not JSON")
    except requests.exceptions.Timeout:
        result["elapsed_sec"] = time.time() - start
        result["anomalies"].append(f"timeout after {timeout}s")
    except requests.exceptions.RequestException as e:
        result["elapsed_sec"] = time.time() - start
        result["anomalies"].append(f"request exception: {e}")
    return result


def print_result(label: str, r: dict, verbose: bool = True) -> None:
    print(f"\n--- {label} ---")
    print(f"  endpoint: {r['endpoint']}")
    print(f"  HTTP {r['http_status']} ({r['elapsed_sec']:.1f}s)")
    if r.get("headers"):
        retry_after = r["headers"].get("Retry-After")
        content_type = r["headers"].get("Content-Type")
        x_backend = r["headers"].get("X-Backend-Id")
        if retry_after:
            print(f"  Retry-After: {retry_after}")
        if content_type:
            print(f"  Content-Type: {content_type}")
        if x_backend:
            print(f"  X-Backend-Id: {x_backend}")
    print(f"  body_len: {len(r['body_text'])}")
    print(f"  is_html: {r['is_html']}, is_json: {r['is_json']}")
    if r.get("parsed"):
        parsed = r["parsed"]
        if "error" in parsed:
            print(f"  ❌ error: {parsed['error'][:200]}")
        if "target_n_ctx" in parsed:
            print(f"  target_n_ctx: {parsed['target_n_ctx']}")
        if "suggestion" in parsed:
            print(f"  suggestion: {parsed['suggestion'][:200]}")
        if "message" in parsed:
            msg = parsed["message"]
            if isinstance(msg, dict):
                content = msg.get("content", "")
                print(f"  content_len: {len(content)}")
                if content:
                    print(f"  content[:100]: {content[:100]!r}")
    if r.get("anomalies"):
        for a in r["anomalies"]:
            print(f"  ⚠️  {a}")


def poll_cppworker_load_state(max_wait_sec: float = 300) -> dict:
    """Poll cppworker до полной загрузки или таймаута."""
    print(f"\n>>> Polling cppworker for load completion (max {max_wait_sec}s)...")
    start = time.time()
    last_state = None
    while time.time() - start < max_wait_sec:
        state = get_cppworker_state()
        cur = state.get("count", 0)
        if cur == 1:
            m = state["models"][0]
            if m.get("state") == "loaded":
                print(f"✓ Model loaded: n_ctx={m.get('context_size')}")
                return state
            else:
                last_state = f"state={m.get('state')}, ctx={m.get('context_size')}"
        else:
            last_state = f"count={cur}"
        elapsed = time.time() - start
        print(f"  T+{elapsed:.0f}s: {last_state}")
        time.sleep(5)
    print(f"✗ Timeout after {max_wait_sec}s, last state: {last_state}")
    return get_cppworker_state()


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--num-ctx", type=int, default=4096)
    p.add_argument("--num-predict", type=int, default=2048)
    p.add_argument("--mode", choices=["nginx", "balancer", "both"], default="both")
    p.add_argument("--max-load-wait", type=int, default=300)
    p.add_argument("--request-timeout", type=int, default=180)
    args = p.parse_args()

    print("=" * 60)
    print(f"R60.42 e2e trace: {MODEL} num_ctx={args.num_ctx} num_predict={args.num_predict}")
    print("=" * 60)

    # Step 1: show current state
    print("\n>>> Initial state:")
    state = get_cppworker_state()
    print(f"    count={state.get('count')}, feasible={state.get('feasible_max_context')}")

    # Step 2: build the body
    body = {
        "model": MODEL,
        "messages": [{"role": "user", "content": PROMPT}],
        "stream": False,
        "options": {
            "num_ctx": args.num_ctx,
            "num_predict": args.num_predict,
            "temperature": 0.7,
        },
    }

    # Step 3: send the request via nginx (mimics OpenWebUI)
    if args.mode in ("nginx", "both"):
        r = send_request_via_nginx(body, args.request_timeout)
        print_result("Result via NGINX (OpenWebUI path)", r)
        # If 503, wait for load then retry
        if r["http_status"] == 503 and "load started" in r.get("parsed", {}).get("error", ""):
            print("\n>>> Got 503 'load started' - waiting for cppworker to finish...")
            poll_cppworker_load_state(args.max_load_wait)
            # Retry
            r = send_request_via_nginx(body, args.request_timeout)
            print_result("Retry via NGINX after load", r)
    # Step 4: send the request via balancer (skips nginx)
    if args.mode in ("balancer", "both"):
        r = send_request_via_balancer(body, args.request_timeout)
        print_result("Result via BALANCER (direct)", r)

    # Final: show cppworker state
    print("\n>>> Final cppworker state:")
    state = get_cppworker_state()
    if state.get("count") == 1:
        m = state["models"][0]
        print(f"    Model: n_ctx={m.get('context_size')}, state={m.get('state')}")
    else:
        print(f"    count={state.get('count')}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
