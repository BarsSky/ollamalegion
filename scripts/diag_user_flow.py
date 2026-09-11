#!/usr/bin/env python3
"""
R60.42 — Симулирует полный user workflow OpenWebUI:
1. User clicks Send (no model loaded yet)
2. Capture: 503 with "load started" message
3. Wait 30s (Retry-After)
4. User retries
5. Capture: either 200 (loaded) or 503 (still loading)
6. Continue until 200 or final failure
7. Show complete timeline

Это точно то что делает OpenWebUI (через nginx).
"""
import json
import sys
import time
import requests

NGINX = "http://localhost:18083"
MODEL = "Qwen3-Instruct-2507-q4km"
PROMPT = (
    "Привет распиши красивый сайт на html css для интерактивной математики "
    "расчета движения полета"
)


def get_cppworker_state() -> dict:
    try:
        r = requests.get("http://localhost:18092/api/models", timeout=5)
        return r.json()
    except Exception as e:
        return {"error": str(e)}


def try_request(num_ctx: int, num_predict: int, timeout: int = 240) -> dict:
    body = {
        "model": MODEL,
        "messages": [{"role": "user", "content": PROMPT}],
        "stream": False,
        "options": {"num_ctx": num_ctx, "num_predict": num_predict, "temperature": 0.7},
    }
    start = time.time()
    try:
        r = requests.post(
            f"{NGINX}/api/chat",
            json=body,
            timeout=timeout,
            headers={"Authorization": "Bearer changeme-bundled-with-agent-token"},
        )
        return {
            "elapsed_sec": time.time() - start,
            "http_status": r.status_code,
            "headers": dict(r.headers),
            "body": r.text,
        }
    except requests.exceptions.Timeout:
        return {"elapsed_sec": time.time() - start, "http_status": 0, "error": f"timeout {timeout}s"}
    except Exception as e:
        return {"elapsed_sec": time.time() - start, "error": str(e)}


def main() -> int:
    print("=" * 60)
    print("R60.42 OpenWebUI user-flow simulation")
    print("=" * 60)
    print(f"Initial cppworker state:")
    state = get_cppworker_state()
    print(f"  count={state.get('count')}")

    body_ctx = 4096
    body_n_predict = 2048

    print(f"\n>>> Attempt 1: User clicks Send")
    print(f"    num_ctx={body_ctx}, num_predict={body_n_predict}")
    attempt = 1
    start_total = time.time()
    while attempt <= 8:  # up to 8 attempts (~8 minutes max)
        r = try_request(body_ctx, body_n_predict)
        status = r.get("http_status", 0)
        elapsed = r.get("elapsed_sec", 0)
        body = r.get("body", "")
        retry_after = r.get("headers", {}).get("Retry-After", "30")
        state = get_cppworker_state()
        cpp_count = state.get("count", 0)
        cpp_state = state["models"][0].get("state") if cpp_count == 1 else "no_model"
        cpp_ctx = state["models"][0].get("context_size") if cpp_count == 1 else 0
        print(f"\n    Attempt {attempt}: HTTP {status} ({elapsed:.1f}s)")
        print(f"      cppworker: count={cpp_count} state={cpp_state} ctx={cpp_ctx}")
        print(f"      Retry-After: {retry_after}")
        if status == 200:
            try:
                d = json.loads(body)
                content = d.get("message", {}).get("content", "")
                print(f"      ✅ SUCCESS: {len(content)} chars, eval_count={d.get('eval_count')}")
                print(f"      done_reason: {d.get('done_reason')}")
                print(f"      content[:100]: {content[:100]!r}")
                total = time.time() - start_total
                print(f"\n>>> TOTAL TIME: {total:.1f}s ({attempt} attempts)")
                return 0
            except Exception as e:
                print(f"      ❌ parse error: {e}")
        elif status == 503:
            # Try to extract info
            try:
                d = json.loads(body)
                err = d.get("error", "")
                sugg = d.get("suggestion", "")
                target = d.get("target_n_ctx", "")
                print(f"      ❌ 503 error: {err[:100]}")
                if target:
                    print(f"      target_n_ctx: {target}")
                if sugg:
                    print(f"      suggestion: {sugg[:150]}")
            except Exception:
                print(f"      ❌ 503 (non-JSON): {body[:200]}")
            # Wait Retry-After seconds
            wait = int(retry_after) if retry_after.isdigit() else 30
            print(f"      ... waiting {wait}s before retry ...")
            time.sleep(wait)
        elif status == 0:
            print(f"      ❌ error: {r.get('error')}")
            time.sleep(30)
        else:
            print(f"      ❌ unexpected status: {body[:200]}")
            time.sleep(30)
        attempt += 1

    total = time.time() - start_total
    print(f"\n>>> FAILED after {attempt-1} attempts in {total:.1f}s")
    return 1


if __name__ == "__main__":
    sys.exit(main())
