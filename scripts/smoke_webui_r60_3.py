#!/usr/bin/env python3
"""
smoke_webui_r60_3.py — R60.3 regression test: webui e2e + per-file meta.

Builds on R60.2 smoke test (11 checks) and adds 3 meta-related checks:
  - /api/models/files returns `size` (alias for sizeBytes)
  - /api/models/files returns `quantization` (parsed from filename)
  - /api/models/files filename `Qwen3-Instruct-2507-q4km.gguf` →
    quantization="Q4_K_M" (compressed alias q4km)
  - /api/models/files still has `sizeBytes` (canonical name, backward compat)

Total: 14 checks (11 from R60.2 + 3 new).

Запуск:
    python scripts/smoke_webui_r60_3.py
    python scripts/smoke_webui_r60_3.py http://localhost:18083

Exit code 0 = PASS, 1 = FAIL.
"""
import asyncio
import json
import sys
import time
from typing import Tuple

import requests
import websockets

# Reuse helpers from R60.2 smoke.
sys.path.insert(0, "scripts")
from smoke_webui_r60_2 import (
    DEFAULT_BASE,
    DEFAULT_TIMEOUT,
    DEFAULT_TOKEN,
    check_endpoint,
    check_inference,
    check_ws_logs,
    check_ws_metrics,
    record,
    results,
    exit_code,
)


def check_models_files_meta(base: str) -> None:
    """R60.3: /api/models/files must include size + quantization per file."""
    try:
        r = requests.get(
            base + "/api/models/files",
            headers={"X-API-Token": DEFAULT_TOKEN},
            timeout=DEFAULT_TIMEOUT,
        )
        if r.status_code != 200:
            record("models/files has size+quantization", False, f"status={r.status_code}")
            return
        body = r.json()
        files = body.get("files", [])
        if not files:
            record("models/files has size+quantization", False, "no files in response")
            return
        first = files[0]
        # Required R60.3 fields
        if "size" not in first:
            record("models/files file has 'size' field", False, f"keys={list(first.keys())}")
        else:
            size = first["size"]
            if size <= 0:
                record("models/files file has 'size' field", False, f"size={size} (not > 0)")
            else:
                record(
                    "models/files file has 'size' field (R60.3 fix)",
                    True,
                    f"name={first.get('name')} size={size}",
                )
        if "sizeBytes" not in first:
            record("models/files file has 'sizeBytes' field", False, f"keys={list(first.keys())}")
        else:
            sb = first["sizeBytes"]
            if sb != first.get("size"):
                record("models/files size==sizeBytes", False, f"size={first.get('size')} sizeBytes={sb}")
            else:
                record(
                    "models/files size == sizeBytes (alias)",
                    True,
                    f"both={sb}",
                )
        if "quantization" not in first:
            record("models/files file has 'quantization' field", False, f"keys={list(first.keys())}")
        else:
            quant = first["quantization"]
            if not quant:
                record(
                    "models/files file has 'quantization' field",
                    False,
                    f"quantization is empty (filename={first.get('name')})",
                )
            else:
                record(
                    "models/files file has 'quantization' field (R60.3 fix)",
                    True,
                    f"name={first.get('name')} → quantization='{quant}'",
                )
        # Specific: Qwen3-Instruct-2507-q4km.gguf should parse to Q4_K_M
        qwen = next((f for f in files if "Qwen3" in f.get("name", "")), None)
        if qwen:
            if qwen.get("quantization") == "Q4_K_M":
                record(
                    "Qwen3-Instruct-2507-q4km.gguf → Q4_K_M",
                    True,
                    f"compressed alias q4km correctly parsed",
                )
            else:
                record(
                    "Qwen3-Instruct-2507-q4km.gguf → Q4_K_M",
                    False,
                    f"got '{qwen.get('quantization')}', want 'Q4_K_M'",
                )
    except Exception as e:
        record("models/files meta checks", False, f"{type(e).__name__}: {e}")


async def run_async_checks(base: str) -> None:
    ok, detail = await check_ws_logs(base)
    record("ws /ws/logs holds 60s", ok, detail)
    ok, detail = await check_ws_metrics(base)
    record("ws /ws/metrics stream", ok, detail)


def main() -> int:
    base = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_BASE
    print(f"[smoke] R60.3 webui e2e → {base}")

    # --- 1. R60.2 baseline endpoints (regression guard) ---
    check_endpoint(base, "/api/v1/backends", ["backends"], "/api/v1/backends")
    check_endpoint(base, "/api/v1/health", ["status"], "/api/v1/health")
    check_endpoint(base, "/api/gpu", ["devices"], "/api/gpu")
    check_endpoint(base, "/api/models/files", ["files", "count"], "/api/models/files (R60.2 fix)")
    check_endpoint(base, "/api/models", ["models", "count"], "/api/models (R60.2 fix)")
    check_endpoint(base, "/api/tags", ["models"], "/api/tags")
    check_endpoint(
        base, "/api/hf/search?query=qwen&limit=2",
        ["query", "results"], "/api/hf/search?query=... (R60.2 fix)",
    )
    check_endpoint(
        base, "/api/v1/proxy/logs?limit=2",
        ["entries", "count"], "/api/v1/proxy/logs?limit=2",
    )

    # --- 2. R60.3 NEW: per-file meta checks ---
    check_models_files_meta(base)

    # --- 3. Inference через webui ---
    check_inference(base)

    # --- 4. WebSocket smoke ---
    asyncio.run(run_async_checks(base))

    # --- 5. Summary ---
    total = len(results["steps"])
    passed = sum(1 for s in results["steps"] if s["ok"])
    failed = total - passed
    print(f"\nResults: {passed}/{total} pass, {failed} fail")

    if results.get("errors"):
        print("\nErrors observed:")
        for e in results["errors"]:
            print(f"  - {e}")

    out = f"test-results/smoke-r60.3-{int(time.time())}.json"
    with open(out, "w", encoding="utf-8") as f:
        json.dump(
            {
                "url": base,
                "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                "passed": passed,
                "failed": failed,
                "steps": results["steps"],
            },
            f,
            indent=2,
        )
    print(f"Report: {out}")

    return exit_code


if __name__ == "__main__":
    sys.exit(main())
