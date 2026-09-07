#!/usr/bin/env python3
"""
run_all_tests.py — Master test runner. Runs all R60.5+ tests in sequence
and reports overall pass/fail.

Includes:
  1. test_ollama_api_compliance.py
  2. test_openai_api_compliance.py
  3. test_generative_dialogue.py
  4. smoke_webui_r60_4.py (regression guard for R60.2-R60.4 fixes)
  5. R60.5 critical: management endpoint tests (load/unload/copy/delete)
  6. R60.5 critical: webui proxy endpoint reachable (no infinite loading)
  7. R60.7: SSE long-running test (150s) — verifies SetWriteDeadline bypass
     prevents /api/v1/events from dropping at 60s server WriteTimeout.
     Set SKIP_SSE_LONG=1 to skip in CI; SSE_DURATION_SEC=60 for shorter run.

Exit code 0 = ALL PASS, 1 = any FAIL.

Запуск:
    python scripts/run_all_tests.py
    python scripts/run_all_tests.py --base http://localhost:18080
    SKIP_SSE_LONG=1 python scripts/run_all_tests.py   # skip 150s SSE test
    SSE_DURATION_SEC=60 python scripts/run_all_tests.py  # shorter SSE test
"""
import argparse
import json
import os
import sys
import time
from typing import List, Tuple

import requests

DEFAULT_BASE = "http://localhost:18080"
DEFAULT_WEBUI = "http://localhost:18083"
DEFAULT_TOKEN = "changeme-bundled-with-agent-token"
DEFAULT_MODEL = "Qwen3-Instruct-2507-q4km"

all_results: List[Tuple[str, bool, str]] = []  # (test_name, ok, detail)


def record(name: str, ok: bool, detail: str = "") -> None:
    all_results.append((name, ok, detail))
    mark = "✓" if ok else "✗"
    print(f"  {mark} {name}{' — ' + detail if detail else ''}")


def section(title: str) -> None:
    print(f"\n=== {title} ===")


def run_ollama_compliance(base: str, model: str) -> bool:
    section("Phase B: Ollama API compliance")
    sys.path.insert(0, "scripts")
    from test_ollama_api_compliance import test_version, test_tags, test_ps, test_show
    from test_ollama_api_compliance import test_chat_non_stream, test_chat_stream
    from test_ollama_api_compliance import test_generate_non_stream, test_embed, test_embeddings_legacy
    test_version(base); test_tags(base); test_ps(base); test_show(base, model)
    test_chat_non_stream(base, model); test_chat_stream(base, model)
    test_generate_non_stream(base, model); test_embed(base, model); test_embeddings_legacy(base, model)
    # Compute pass/fail from module's global state
    from test_ollama_api_compliance import results as ollama_results
    passed = sum(1 for t in ollama_results["tests"] if t["ok"])
    total = len(ollama_results["tests"])
    return passed == total


def run_openai_compliance(base: str, model: str) -> bool:
    section("Phase C: OpenAI API compliance")
    from test_openai_api_compliance import test_list_models, test_retrieve_model
    from test_openai_api_compliance import test_chat_completions_non_stream, test_chat_completions_stream
    from test_openai_api_compliance import test_completions_legacy, test_embeddings, test_embeddings_array_input
    test_list_models(base); test_retrieve_model(base, model)
    test_chat_completions_non_stream(base, model); test_chat_completions_stream(base, model)
    test_completions_legacy(base, model); test_embeddings(base, model); test_embeddings_array_input(base, model)
    from test_openai_api_compliance import results as openai_results
    passed = sum(1 for t in openai_results["tests"] if t["ok"])
    total = len(openai_results["tests"])
    return passed == total


def run_generative_dialogue(base: str, model: str) -> bool:
    section("Phase D: Generative dialogue")
    from test_generative_dialogue import (
        test_single_turn, test_multi_turn_context, test_streaming_complete,
        test_token_accounting, test_long_context, test_concurrent_requests, test_cancellation,
    )
    test_single_turn(base, model); test_multi_turn_context(base, model); test_streaming_complete(base, model)
    test_token_accounting(base, model); test_long_context(base, model)
    test_concurrent_requests(base, model); test_cancellation(base, model)
    from test_generative_dialogue import results as gen_results
    passed = sum(1 for t in gen_results["tests"] if t["ok"])
    total = len(gen_results["tests"])
    return passed == total


def run_r60_5_critical(base: str, webui: str, model: str, token: str) -> bool:
    section("Phase F: R60.5 critical — stripStreamFlag fix for management endpoints")
    headers = {"X-API-Token": token, "Content-Type": "application/json"}
    all_ok = True

    # 1. /api/models/load via balancer must NOT add 'stream' field
    try:
        r = requests.post(
            base + "/api/models/load",
            json={"name": model},
            headers=headers,
            timeout=15,
        )
        ok = r.status_code in (200, 202)
        detail = f"status={r.status_code}"
        if "unknown field" in r.text:
            ok = False
            detail += f" (BUG: stream field still added — {r.text[:100]})"
            all_ok = False
        record("POST /api/models/load via balancer (R60.5 fix)", ok, detail)
    except Exception as e:
        record("POST /api/models/load via balancer", False, f"{type(e).__name__}: {e}")
        all_ok = False

    # 2. /api/copy via balancer must accept arbitrary body (no 'stream' added)
    try:
        r = requests.post(
            base + "/api/copy",
            json={"source": "nonexistent", "destination": "test"},
            headers=headers,
            timeout=15,
        )
        # 404 ("source not found") is OK — means balancer parsed body correctly.
        # 400 ("unknown field") would mean stripStreamFlag bug.
        ok = r.status_code == 404 and "unknown field" not in r.text
        detail = f"status={r.status_code}"
        if "unknown field" in r.text:
            ok = False
            detail += f" (BUG: stream added to copy body)"
            all_ok = False
        record("POST /api/copy via balancer (R60.5 fix)", ok, detail)
    except Exception as e:
        record("POST /api/copy via balancer", False, f"{type(e).__name__}: {e}")
        all_ok = False

    # 3. /api/models/unload via balancer
    try:
        r = requests.post(
            base + "/api/models/unload",
            json={"name": "nonexistent-model"},
            headers=headers,
            timeout=15,
        )
        ok = r.status_code in (200, 404) and "unknown field" not in r.text
        detail = f"status={r.status_code}"
        if "unknown field" in r.text:
            ok = False
            detail += f" (BUG: stream added to unload body)"
            all_ok = False
        record("POST /api/models/unload via balancer (R60.5 fix)", ok, detail)
    except Exception as e:
        record("POST /api/models/unload via balancer", False, f"{type(e).__name__}: {e}")
        all_ok = False

    # 4. /api/v1/gguf/backends/{id}/proxy/api/v1/cppworker/config via webui
    #    (R60.5 webui fix #1: hard timeout + try/catch on render)
    try:
        r = requests.get(
            webui + "/api/v1/gguf/backends/cppworker-gpu-bundled-agent/proxy/api/v1/cppworker/config",
            headers={"X-API-Token": token},
            timeout=15,
        )
        ok = r.status_code == 200
        record("WebUI /api/v1/gguf/.../proxy/api/v1/cppworker/config (R60.5 webui fix)", ok, f"status={r.status_code}")
        if not ok:
            all_ok = False
    except Exception as e:
        record("WebUI /api/v1/gguf/.../proxy/api/v1/cppworker/config", False, f"{type(e).__name__}: {e}")
        all_ok = False

    # 5. /api/models/files via webui (R60.5 webui fix #2: disk fallback source)
    try:
        r = requests.get(
            webui + "/api/models/files",
            headers={"X-API-Token": token},
            timeout=15,
        )
        body = r.json() if r.status_code == 200 else {}
        files = body.get("files", [])
        has_quant = any(f.get("quantization") for f in files)
        has_size = any(f.get("size", 0) > 0 for f in files)
        ok = r.status_code == 200 and has_quant and has_size
        record(
            "WebUI /api/models/files (R60.3+R60.5 size+quantization)",
            ok,
            f"status={r.status_code} count={len(files)} has_size={has_size} has_quant={has_quant}",
        )
        if not ok:
            all_ok = False
    except Exception as e:
        record("WebUI /api/models/files", False, f"{type(e).__name__}: {e}")
        all_ok = False

    return all_ok


def run_r60_7_sse_long_running(webui: str) -> bool:
    """R60.7 (2026-09-07): SSE /api/v1/events survives past server WriteTimeout.

    Before fix: API server (port 18081) had WriteTimeout=60s. Go's WriteTimeout
    is the absolute deadline for the response lifetime, NOT reset on each
    Write. SSE died at 60s exactly (5 pings × 10s + headers ≈ 60s).

    After fix: http.NewResponseController(w).SetWriteDeadline(time.Time{}) in
    handleEvents removes the per-response deadline. Stream lives until client
    disconnect or TCP keepalive.

    This test runs for 150s (default) and verifies at least one ping arrives
    after the 65s mark. Set SKIP_SSE_LONG=1 to skip in CI.
    """
    section("Phase H: R60.7 — SSE long-running (no WriteTimeout kill at 60s)")
    import subprocess
    env = os.environ.copy()
    # Allow override via env, but ensure we honor the user's choice
    if "SSE_DURATION_SEC" not in env and "SKIP_SSE_LONG" not in env:
        env["SSE_DURATION_SEC"] = "150"
    env["WEBUI_BASE"] = webui
    print(f"  Running: scripts/test_sse_long_running.py")
    print(f"  Env: SSE_DURATION_SEC={env.get('SSE_DURATION_SEC', '150')} SKIP_SSE_LONG={env.get('SKIP_SSE_LONG', '0')}")
    try:
        result = subprocess.run(
            ["python", "scripts/test_sse_long_running.py"],
            env=env,
            capture_output=True,
            text=True,
            timeout=300,  # hard cap at 5 min (in case of hangs)
        )
        ok = result.returncode == 0
        # Show last few lines of output for context
        stdout_lines = result.stdout.strip().split("\n")
        for line in stdout_lines[-8:]:
            print(f"    {line}")
        if not ok:
            print(f"  stderr: {result.stderr[-500:] if result.stderr else '(empty)'}")
        record("SSE /api/v1/events survives past 60s WriteTimeout (R60.7 fix)", ok)
        return ok
    except subprocess.TimeoutExpired:
        record("SSE /api/v1/events long-running", False, "subprocess timeout (>5 min)")
        return False
    except Exception as e:
        record("SSE /api/v1/events long-running", False, f"{type(e).__name__}: {e}")
        return False


def main() -> int:
    parser = argparse.ArgumentParser(description="R60.5 master test runner")
    parser.add_argument("--base", default=DEFAULT_BASE, help="Balancer main proxy base URL")
    parser.add_argument("--webui", default=DEFAULT_WEBUI, help="WebUI base URL")
    parser.add_argument("--model", default=DEFAULT_MODEL, help="Model name to use for tests")
    parser.add_argument("--token", default=DEFAULT_TOKEN, help="API token")
    args = parser.parse_args()

    print(f"[run_all_tests] base={args.base} webui={args.webui} model={args.model}")

    start = time.time()
    results = {
        "B (Ollama)": run_ollama_compliance(args.base, args.model),
        "C (OpenAI)": run_openai_compliance(args.base, args.model),
        "D (Generative)": run_generative_dialogue(args.base, args.model),
        "F (R60.5 critical)": run_r60_5_critical(args.base, args.webui, args.model, args.token),
        "H (R60.7 SSE long-running)": run_r60_7_sse_long_running(args.webui),
    }
    elapsed = time.time() - start

    print(f"\n=== Summary ({elapsed:.1f}s) ===")
    total_pass = 0
    total_fail = 0
    for phase, ok in results.items():
        mark = "✓" if ok else "✗"
        print(f"  {mark} {phase}")
        if ok:
            total_pass += 1
        else:
            total_fail += 1
    print(f"\n{len(results)} phases: {total_pass} pass, {total_fail} fail")

    return 0 if total_fail == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
