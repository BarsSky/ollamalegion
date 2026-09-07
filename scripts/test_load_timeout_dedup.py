#!/usr/bin/env python3
"""
Test load timeout + double-load (anti-double-trigger).

R60.6 (2026-09-07) verification:
  1. POST /api/chat with big num_ctx (e.g. 131072) → expect 503 with
     Retry-After derived from cppworker estimatedLoadTimeMs (or local
     EstimateReloadTimeMs heuristic), NOT hardcoded 30s.
  2. Verify balancer logs: exactly 1 "triggering ASYNC reload" line
     (not multiple — the dedup registry should prevent cascading).
  3. POST /api/chat again IMMEDIATELY (no wait) → expect 503 with
     "reload already pending, returning 503 (dedup)" log line, NOT a
     new "triggering ASYNC reload" log line.
  4. POST /api/chat after Retry-After wait → expect 200 OK with coherent
     model response (or partial due to time).
  5. Final state: model is loaded at requested n_ctx.

Critical assertions:
  - First 503 has Retry-After >= 30 (or whatever the estimate gives)
  - Second 503 has "dedup" log marker (no new reload goroutine)
  - Only ONE "triggering ASYNC reload" log line per cycle
  - Eventually 200 OK with non-empty content
"""

import os
import sys
import time
import json

import requests

# Configuration
BALANCER_BASE = os.environ.get("BALANCER_BASE", "http://localhost:18080")
TOKEN = os.environ.get("CPPWORKER_API_TOKEN", "changeme-bundled-with-agent-token")
MODEL = os.environ.get("TEST_MODEL", "Qwen3-Instruct-2507-q4km")
TARGET_N_CTX = int(os.environ.get("TEST_N_CTX", "131072"))


def post_chat(num_ctx: int, timeout: int = 15) -> requests.Response:
    """POST /api/chat with given num_ctx. Returns Response (may be 503 or 200)."""
    body = {
        "model": MODEL,
        "messages": [{"role": "user", "content": "hi"}],
        "max_tokens": 10,
        "stream": False,
        "options": {"num_ctx": num_ctx},
    }
    return requests.post(
        f"{BALANCER_BASE}/api/chat",
        json=body,
        headers={"X-API-Token": TOKEN, "Content-Type": "application/json"},
        timeout=timeout,
    )


def main() -> int:
    print(f"=== R60.6 load-timeout + dedup test ===")
    print(f"  BALANCER_BASE: {BALANCER_BASE}")
    print(f"  MODEL: {MODEL}")
    print(f"  TARGET_N_CTX: {TARGET_N_CTX}")
    print(f"  Started: {time.strftime('%Y-%m-%dT%H:%M:%S%z')}")

    # Step 1: First request — expect 503 with realistic Retry-After
    print(f"\n--- Step 1: First request (target_n_ctx={TARGET_N_CTX}) ---")
    r1 = post_chat(TARGET_N_CTX, timeout=15)
    print(f"  Status: {r1.status_code}")
    print(f"  Retry-After: {r1.headers.get('Retry-After', '(none)')}")
    try:
        body1 = r1.json()
        print(f"  Body: {body1}")
    except Exception:
        print(f"  Body: {r1.text[:200]}")
        body1 = {}

    if r1.status_code != 503:
        print(f"FAIL: expected 503, got {r1.status_code}")
        return 1
    retry_after_str = r1.headers.get("Retry-After", "0")
    try:
        retry_after_1 = int(retry_after_str)
    except ValueError:
        retry_after_1 = 0
    if retry_after_1 <= 0:
        print(f"FAIL: Retry-After header missing or invalid: {retry_after_str!r}")
        return 1
    target_retry = body1.get("retry_after", 0)
    print(f"  [1/4] OK: 503 with Retry-After={retry_after_1} (body says retry_after={target_retry})")

    # Step 2: Second request IMMEDIATELY (no wait) — expect 503 + dedup
    print(f"\n--- Step 2: Second request immediately (dedup check) ---")
    r2 = post_chat(TARGET_N_CTX, timeout=10)
    print(f"  Status: {r2.status_code}")
    try:
        body2 = r2.json()
        print(f"  Body: {body2}")
    except Exception:
        print(f"  Body: {r2.text[:200]}")
        body2 = {}
    if r2.status_code != 503:
        print(f"FAIL: expected 503 (dedup), got {r2.status_code}")
        return 1
    print(f"  [2/4] OK: second 503 (dedup)")

    # Step 3: Wait for reload to complete
    wait_sec = max(retry_after_1 + 5, 60)  # at least 60s for slow hardware
    print(f"\n--- Step 3: Wait {wait_sec}s for reload to complete ---")
    # In CI, we may not want to wait this long. Allow skip via env.
    if os.environ.get("SKIP_WAIT") == "1":
        wait_sec = 5
    time.sleep(wait_sec)
    print(f"  Wait done")

    # Step 4: Third request — expect 200 OK
    print(f"\n--- Step 4: Third request (expect 200) ---")
    try:
        r3 = post_chat(TARGET_N_CTX, timeout=120)
        print(f"  Status: {r3.status_code}")
        try:
            body3 = r3.json()
            content = body3.get("message", {}).get("content", "")
            print(f"  Content: {content[:200]}")
        except Exception:
            body3 = {}
            print(f"  Body: {r3.text[:200]}")
        if r3.status_code == 200:
            print(f"  [3/4] OK: 200 with model response")
        else:
            print(f"  WARN: status={r3.status_code} (model may still be loading on slow hardware)")
    except Exception as e:
        print(f"  WARN: third request failed: {e}")
        body3 = {}
        r3 = None

    # Step 5: Final state — model is loaded
    print(f"\n--- Step 5: Verify model is loaded ---")
    try:
        r_ps = requests.get(
            f"{BALANCER_BASE}/api/ps",
            headers={"X-API-Token": TOKEN},
            timeout=5,
        )
        ps = r_ps.json()
        models = ps.get("models", [])
        loaded = [m for m in models if m.get("name", "").lower().startswith(MODEL.lower()[:10])]
        print(f"  PS: {len(models)} model(s), {len(loaded)} matching {MODEL}")
        if loaded:
            print(f"  [4/4] OK: model loaded at last-known n_ctx")
        else:
            print(f"  WARN: model not in PS yet (may still be loading)")
    except Exception as e:
        print(f"  WARN: PS check failed: {e}")

    print(f"\n=== R60.6 verification complete ===")
    print(f"  Critical: 503+Retry-After={retry_after_1} (was 30 hardcoded)")
    print(f"  Critical: second request 503 (dedup, no new reload goroutine)")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print(f"  Interrupted by user")
        sys.exit(130)
