#!/usr/bin/env python3
"""
smoke_webui_r60_4.py — R60.4 regression test: webui meta on loaded models.

Builds on R60.3 smoke test (15 checks) and adds 4 new checks for the
loadedModels meta in /api/v1/backends/{id}:

  - /api/v1/backends/{id}.llamaCpp.loadedModels has non-empty size
    (R60.4: os.Stat fallback fills size when gguf meta reports 0)
  - /api/v1/backends/{id}.llamaCpp.loadedModels has non-empty quantization
    (R60.4: parseQuantization fills from path)
  - /api/v1/backends/{id}.llamaCpp.loadedModels has non-empty path
    (R60.4: modelPath added to notify callback)
  - loadedModels[0] has all 3 fields populated simultaneously
    (real-world scenario for webui card)

Total: 19 checks (15 from R60.3 + 4 new).

Запуск:
    python scripts/smoke_webui_r60_4.py
    python scripts/smoke_webui_r60_4.py http://localhost:18083

Exit code 0 = PASS, 1 = FAIL.

Note: This test requires a model to be loaded in VRAM. If no model is
loaded (count=0), the R60.4-specific checks fail with a clear message.
Use scripts/load_then_smoke.sh to first trigger a load.
"""
import asyncio
import json
import sys
import time

import requests

sys.path.insert(0, "scripts")
from smoke_webui_r60_2 import (
    DEFAULT_BASE,
    DEFAULT_TIMEOUT,
    DEFAULT_TOKEN,
    results,
    exit_code,
    record,
)
from smoke_webui_r60_3 import (
    check_models_files_meta,
    check_endpoint,
    check_inference,
)


def check_loaded_models_meta(base: str) -> None:
    """R60.4: /api/v1/backends/{id} must return loadedModels with full meta."""
    try:
        r = requests.get(
            base + "/api/v1/backends/cppworker-gpu-bundled-agent",
            headers={"X-API-Token": DEFAULT_TOKEN},
            timeout=DEFAULT_TIMEOUT,
        )
        if r.status_code != 200:
            record("/api/v1/backends/{id} loadedModels meta", False, f"status={r.status_code}")
            return
        body = r.json()
        llama = body.get("llamaCpp", {})
        loaded = llama.get("loadedModels", [])
        if not loaded:
            record(
                "/api/v1/backends/{id} loadedModels meta (R60.4)",
                False,
                "no loaded models — first trigger a load (POST /api/models/load) and wait 60s",
            )
            return
        m = loaded[0]

        # === R60.4 NEW: size, quantization, path populated together ===
        size = m.get("size", 0)
        if size > 0:
            record(
                "loadedModels[0].size > 0 (R60.4 os.Stat fallback)",
                True,
                f"size={size} ({size / 1024 / 1024:.0f} MB) — R60.4 fix: real file size, not gguf meta",
            )
        else:
            record(
                "loadedModels[0].size > 0 (R60.4 os.Stat fallback)",
                False,
                f"size={size} — R60.4 fix not applied; check notifyModelLoaded callback",
            )

        quant = m.get("quantization", "")
        if quant:
            record(
                "loadedModels[0].quantization is set (R60.4 parseQuantization)",
                True,
                f"quantization='{quant}' — R60.4: parse from path",
            )
        else:
            record(
                "loadedModels[0].quantization is set (R60.4 parseQuantization)",
                False,
                f"quantization='{quant}' — R60.4 fix not applied",
            )

        path = m.get("path", "")
        if path and path.endswith(".gguf"):
            record(
                "loadedModels[0].path is set (R60.4 notify modelPath)",
                True,
                f"path='{path}' — R60.4: modelPath added to callback",
            )
        else:
            record(
                "loadedModels[0].path is set (R60.4 notify modelPath)",
                False,
                f"path='{path}' — R60.4 fix not applied",
            )

        # === Combined: all 3 R60.4 fields present → webui card works ===
        all_ok = size > 0 and bool(quant) and bool(path) and path.endswith(".gguf")
        if all_ok:
            record(
                "R60.4 loadedModels full meta (webui card ready)",
                True,
                f"name='{m.get('name')}' size={size // 1024 // 1024}MB quant='{quant}'",
            )
        else:
            missing = []
            if size == 0:
                missing.append("size")
            if not quant:
                missing.append("quantization")
            if not path:
                missing.append("path")
            record(
                "R60.4 loadedModels full meta (webui card ready)",
                False,
                f"missing: {missing}",
            )
    except Exception as e:
        record("/api/v1/backends/{id} loadedModels meta", False, f"{type(e).__name__}: {e}")


async def run_async_checks(base: str) -> None:
    """Reuse WS smoke from R60.2."""
    from smoke_webui_r60_2 import check_ws_logs, check_ws_metrics

    ok, detail = await check_ws_logs(base)
    record("ws /ws/logs holds 60s", ok, detail)
    ok, detail = await check_ws_metrics(base)
    record("ws /ws/metrics stream", ok, detail)


def main() -> int:
    base = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_BASE
    print(f"[smoke] R60.4 webui e2e → {base}")

    # --- 1. R60.2 baseline ---
    check_endpoint(base, "/api/v1/backends", ["backends"], "/api/v1/backends")
    check_endpoint(base, "/api/v1/health", ["status"], "/api/v1/health")
    check_endpoint(base, "/api/gpu", ["devices"], "/api/gpu")
    check_endpoint(base, "/api/models/files", ["files", "count"], "/api/models/files (R60.2)")
    check_endpoint(base, "/api/models", ["models", "count"], "/api/models (R60.2)")
    check_endpoint(base, "/api/tags", ["models"], "/api/tags")
    check_endpoint(
        base, "/api/hf/search?query=qwen&limit=2",
        ["query", "results"], "/api/hf/search?query=... (R60.2)",
    )
    check_endpoint(
        base, "/api/v1/proxy/logs?limit=2",
        ["entries", "count"], "/api/v1/proxy/logs?limit=2",
    )

    # --- 2. R60.3 NEW: per-file meta ---
    check_models_files_meta(base)

    # --- 3. R60.4 NEW: loaded models meta (not just disk files) ---
    check_loaded_models_meta(base)

    # --- 4. Inference через webui ---
    check_inference(base)

    # --- 5. WebSocket smoke ---
    asyncio.run(run_async_checks(base))

    # --- 6. Summary ---
    total = len(results["steps"])
    passed = sum(1 for s in results["steps"] if s["ok"])
    failed = total - passed
    print(f"\nResults: {passed}/{total} pass, {failed} fail")

    out = f"test-results/smoke-r60.4-{int(time.time())}.json"
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
