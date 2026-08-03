#!/usr/bin/env python3
"""
run_all.py — master test runner for bundled verify suite.

Запускает все verify-тесты в tests/verify_bundled/ и выводит сводный отчёт.

Usage: python run_all.py [base_url] [token]
Default: http://localhost:18092, changeme-bundled-strong-token-please-change

Exit codes:
  0 — all tests passed
  1 — some tests failed
  2 — health check failed (bundled not running)

Этот скрипт можно подключить к:
  - Makefile target: `make verify-bundled`
  - CI: `python tests/verify_bundled/run_all.py`
  - Cron: regular regression checks
"""
import os
import subprocess
import sys
import time
from typing import Dict, List, Tuple

URL = sys.argv[1] if len(sys.argv) > 1 else "http://localhost:18092"
TOKEN = sys.argv[2] if len(sys.argv) > 2 else "changeme-bundled-strong-token-please-change"
HERE = os.path.dirname(os.path.abspath(__file__))


def http_health_check() -> bool:
    import urllib.request
    try:
        req = urllib.request.Request(
            f"{URL}/v1/models",
            headers={"Authorization": f"Bearer {TOKEN}"},
            method="GET",
        )
        with urllib.request.urlopen(req, timeout=5) as resp:
            return resp.status == 200
    except Exception:
        return False


def run_test_script(name: str, script: str) -> Tuple[int, str]:
    """Run a test script and return (exit_code, output)."""
    print(f"\n{'#' * 60}")
    print(f"# Running: {name}")
    print(f"# Script: {script}")
    print(f"{'#' * 60}")
    started = time.time()
    try:
        result = subprocess.run(
            ["python", script, URL, TOKEN],
            cwd=os.path.dirname(HERE),  # run from repo root
            capture_output=True,
            text=True,
            timeout=900,  # 15 min max per test
        )
        elapsed = time.time() - started
        output = (result.stdout or "") + (result.stderr or "")
        print(output)
        print(f"\n[{name}] exit={result.returncode}, elapsed={elapsed:.1f}s")
        return result.returncode, output
    except subprocess.TimeoutExpired:
        elapsed = time.time() - started
        print(f"\n[{name}] TIMEOUT after {elapsed:.1f}s")
        return 1, "TIMEOUT"
    except Exception as e:
        print(f"\n[{name}] ERROR: {e}")
        return 1, str(e)


def main():
    print("=" * 60)
    print(f"Bundled Verify Suite @ {URL}")
    print(f"Time: {time.strftime('%Y-%m-%d %H:%M:%S')}")
    print("=" * 60)

    # Health check
    print("\nPre-flight: checking bundled health...")
    if not http_health_check():
        print(f"  Health check FAILED. Bundled not running at {URL}?")
        print("  Start with: cd C:/Ollama/ollamalegion && scripts/start-bundled.ps1")
        return 2
    print("  Health check OK")

    # Run all test scripts
    scripts = [
        ("streaming",     os.path.join(HERE, "test_streaming.py")),
        ("reasoning",     os.path.join(HERE, "test_reasoning.py")),
        ("parallel",      os.path.join(HERE, "test_parallel.py")),
        ("api-coverage",  os.path.join(HERE, "test_api_coverage.py")),  # Round 21
    ]

    results: Dict[str, Tuple[int, str]] = {}
    for name, script in scripts:
        if not os.path.exists(script):
            print(f"\nWARN: {script} not found, skipping")
            results[name] = (1, "not found")
            continue
        results[name] = run_test_script(name, script)

    # Summary
    print("\n" + "=" * 60)
    print("BUNDLED VERIFY SUITE — SUMMARY")
    print("=" * 60)
    passed = 0
    total = len(results)
    for name, (code, _) in results.items():
        marker = "PASS" if code == 0 else "FAIL"
        print(f"  {marker}: {name}")
        if code == 0:
            passed += 1
    print(f"\n  {passed}/{total} test groups passed")
    return 0 if passed == total else 1


if __name__ == "__main__":
    sys.exit(main())
