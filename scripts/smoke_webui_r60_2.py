#!/usr/bin/env python3
"""
smoke_webui_r60_2.py — live e2e regression test для R60.2 webui nginx fix.

Проверяет что R60.2 fix работает в боевых условиях:
- /api/models/files возвращает список GGUF (не 404)
- /api/models показывает loaded модели
- /api/hf/search пробрасывает query string (не теряет)
- /api/v1/proxy/logs доступен через webui proxy
- /ws/logs WS connection держится >60s (R60.2 увеличил proxy_send_timeout)
- /ws/metrics stream присылает updates
- /openai/v1/chat/completions inference через webui → 200

Запуск:
    python scripts/smoke_webui_r60_2.py
    python scripts/smoke_webui_r60_2.py http://localhost:18083

Exit code 0 = PASS, 1 = FAIL.
"""
import asyncio
import json
import sys
import time
from typing import Tuple

import requests
import websockets

DEFAULT_BASE = "http://localhost:18083"
DEFAULT_TOKEN = "changeme-bundled-with-agent-token"
DEFAULT_TIMEOUT = 30

results = {"steps": [], "errors": []}
exit_code = 0


def record(name: str, ok: bool, detail: str = "") -> None:
    global exit_code
    results["steps"].append({"name": name, "ok": ok, "detail": detail})
    mark = "✓" if ok else "✗"
    print(f"  {mark} {name}{' — ' + detail if detail else ''}")
    if not ok:
        exit_code = 1


def check_endpoint(base: str, path: str, expect_keys: list, label: str) -> None:
    try:
        r = requests.get(
            base + path,
            headers={"X-API-Token": DEFAULT_TOKEN},
            timeout=DEFAULT_TIMEOUT,
        )
        if r.status_code != 200:
            record(label, False, f"status={r.status_code} body={r.text[:100]}")
            return
        body = r.json()
        missing = [k for k in expect_keys if k not in body]
        if missing:
            record(label, False, f"missing keys {missing} in {list(body.keys())}")
            return
        record(label, True, f"keys={list(body.keys())[:5]}")
    except Exception as e:
        record(label, False, f"{type(e).__name__}: {e}")


def check_inference(base: str) -> None:
    """POST /openai/v1/chat/completions с минимальным запросом."""
    body = {
        "model": "Qwen3-Instruct-2507-q4km",
        "messages": [{"role": "user", "content": "Say 'ok'"}],
        "max_tokens": 5,
        "stream": False,
    }
    try:
        r = requests.post(
            base + "/openai/v1/chat/completions",
            headers={
                "X-API-Token": DEFAULT_TOKEN,
                "Content-Type": "application/json",
            },
            json=body,
            timeout=60,
        )
        if r.status_code != 200:
            record("inference /openai/v1/chat/completions", False, f"status={r.status_code}")
            return
        j = r.json()
        text = j.get("choices", [{}])[0].get("message", {}).get("content", "")
        record(
            "inference /openai/v1/chat/completions",
            len(text) > 0,
            f"content='{text[:40]}' tokens={j.get('usage', {}).get('total_tokens', 'n/a')}",
        )
    except Exception as e:
        record("inference /openai/v1/chat/completions", False, f"{type(e).__name__}: {e}")


async def check_ws_logs(base: str) -> Tuple[bool, str]:
    """WS /ws/logs держится >60s, snapshot приходит сразу."""
    ws_url = base.replace("http://", "ws://") + f"/ws/logs?token={DEFAULT_TOKEN}"
    start = time.time()
    try:
        async with websockets.connect(ws_url, ping_interval=20) as ws:
            try:
                snap = await asyncio.wait_for(ws.recv(), timeout=5.0)
            except asyncio.TimeoutError:
                return False, "no snapshot in 5s"
            # Hold for 60s, allow pings
            held = 0.0
            ping_count = 0
            while time.time() - start < 60:
                try:
                    msg = await asyncio.wait_for(ws.recv(), timeout=10.0)
                    if '"type":"ping"' in msg:
                        ping_count += 1
                except asyncio.TimeoutError:
                    pass
                held = time.time() - start
            return True, f"snapshot OK, held {held:.1f}s, pings={ping_count}"
    except Exception as e:
        return False, f"{type(e).__name__}: {e}"


async def check_ws_metrics(base: str) -> Tuple[bool, str]:
    """WS /ws/metrics отдаёт хотя бы 1 metric update."""
    ws_url = base.replace("http://", "ws://") + f"/ws/metrics?token={DEFAULT_TOKEN}"
    try:
        async with websockets.connect(ws_url, ping_interval=20) as ws:
            for i in range(3):
                try:
                    msg = await asyncio.wait_for(ws.recv(), timeout=5.0)
                    j = json.loads(msg)
                    data = j.get("data", {})
                    return True, f"healthy={data.get('healthyBackends')} gpu={data.get('totalGpuUsage')}%"
                except asyncio.TimeoutError:
                    continue
        return False, "no metrics in 15s"
    except Exception as e:
        return False, f"{type(e).__name__}: {e}"


async def run_async_checks(base: str) -> None:
    ok, detail = await check_ws_logs(base)
    record("ws /ws/logs holds 60s", ok, detail)
    ok, detail = await check_ws_metrics(base)
    record("ws /ws/metrics stream", ok, detail)


def main() -> int:
    base = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_BASE
    print(f"[smoke] R60.2 webui e2e → {base}")

    # --- 1. CRUD endpoints (read) ---
    check_endpoint(base, "/api/v1/backends", ["backends"], "/api/v1/backends")
    check_endpoint(base, "/api/v1/health", ["status"], "/api/v1/health")
    check_endpoint(base, "/api/gpu", ["devices"], "/api/gpu")
    check_endpoint(base, "/api/models/files", ["files", "count"], "/api/models/files (R60.2 fix)")
    check_endpoint(base, "/api/models", ["models", "count"], "/api/models (R60.2 fix)")
    check_endpoint(base, "/api/tags", ["models"], "/api/tags")
    # R60.2: HF search через nginx → cppworker напрямую (балансер терял query)
    check_endpoint(
        base, "/api/hf/search?query=qwen&limit=2",
        ["query", "results"], "/api/hf/search?query=... (R60.2 fix)",
    )
    # R60.2: proxy logs REST
    check_endpoint(
        base, "/api/v1/proxy/logs?limit=2",
        ["entries", "count"], "/api/v1/proxy/logs?limit=2",
    )

    # --- 2. Inference через webui → main proxy → cppworker ---
    check_inference(base)

    # --- 3. WebSocket smoke ---
    asyncio.run(run_async_checks(base))

    # --- 4. Summary ---
    total = len(results["steps"])
    passed = sum(1 for s in results["steps"] if s["ok"])
    failed = total - passed
    print(f"\nResults: {passed}/{total} pass, {failed} fail")

    if results["errors"]:
        print("\nErrors observed:")
        for e in results["errors"]:
            print(f"  - {e}")

    out = f"test-results/smoke-r60.2-{int(time.time())}.json"
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
