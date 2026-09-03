"""Headless diagnostic of WebUI: открывает http://localhost:18083/, ловит все API
запросы и console errors, делает скриншот, сохраняет HTML."""
import asyncio
import json
import sys
from pathlib import Path

from playwright.async_api import async_playwright

OUT = Path("C:/Ollama/ollamalegion/_diag")
OUT.mkdir(parents=True, exist_ok=True)


async def main():
    async with async_playwright() as p:
        browser = await p.chromium.launch(headless=True)
        ctx = await browser.new_context(
            viewport={"width": 1400, "height": 900},
            ignore_https_errors=True,
        )
        page = await ctx.new_page()

        api_reqs = []
        api_resps = []
        console_logs = []
        page_errors = []
        failed_reqs = []

        page.on("request", lambda r: api_reqs.append({
            "url": r.url,
            "method": r.method,
            "headers": dict(r.headers),
        }) if "/api/" in r.url else None)

        page.on("response", lambda r: api_resps.append({
            "url": r.url,
            "status": r.status,
            "status_text": r.status_text,
        }) if "/api/" in r.url else None)

        page.on("console", lambda msg: console_logs.append({
            "type": msg.type,
            "text": msg.text,
        }))

        page.on("pageerror", lambda err: page_errors.append(str(err)))

        page.on("requestfailed", lambda r: failed_reqs.append({
            "url": r.url,
            "failure": r.failure,
        }))

        try:
            await page.goto("http://localhost:18083/", wait_until="networkidle", timeout=15000)
        except Exception as e:
            print(f"[!] goto error: {e}")

        # wait extra 3 sec for any periodic refresh
        await asyncio.sleep(3)

        # Screenshot full page
        shot = OUT / "webui-dashboard.png"
        await page.screenshot(path=str(shot), full_page=True)
        print(f"[+] Screenshot saved: {shot}")

        # Save HTML
        html = await page.content()
        (OUT / "webui-dashboard.html").write_text(html, encoding="utf-8")
        print(f"[+] HTML saved: {OUT / 'webui-dashboard.html'}")

        # Title and visible h1
        title = await page.title()
        h1 = await page.evaluate("() => document.querySelector('h1, h2, .page-title')?.textContent || ''")
        print(f"[i] Title: {title!r}")
        print(f"[i] h1: {h1!r}")

        # Connection status badge
        cs = await page.evaluate("""
            () => {
                const el = document.getElementById('connectionStatus')
                    || document.querySelector('[id*=connection]')
                    || document.querySelector('.connection-status');
                return el ? el.textContent.trim() : 'NOT FOUND';
            }
        """)
        print(f"[i] connection status element: {cs!r}")

        # Auth-related check
        auth_check = await page.evaluate("""
            () => ({
                config: window.WEBUI_CONFIG,
                tokenLen: (window.WEBUI_CONFIG?.API_TOKEN || '').length,
                apiLayerExists: typeof window.Api,
                healthFn: typeof (window.Api && window.Api.health),
            })
        """)
        print(f"[i] auth check: {json.dumps(auth_check, ensure_ascii=False)}")

        # Summary
        print()
        print("=" * 60)
        print(f"[API REQUESTS] {len(api_reqs)}")
        for r in api_reqs[:15]:
            x_tok = r["headers"].get("x-api-token", "MISSING")
            print(f"  {r['method']} {r['url']}  X-API-Token={x_tok[:8] + '...' if x_tok != 'MISSING' else 'MISSING'}")
        print()
        print(f"[API RESPONSES] {len(api_resps)}")
        for r in api_resps[:15]:
            print(f"  {r['status']} {r['url']}")
        print()
        print(f"[CONSOLE LOGS] {len(console_logs)}")
        for c in console_logs[:15]:
            print(f"  [{c['type']}] {c['text'][:200]}")
        print()
        print(f"[PAGE ERRORS] {len(page_errors)}")
        for e in page_errors[:10]:
            print(f"  {e[:300]}")
        print()
        print(f"[FAILED REQUESTS] {len(failed_reqs)}")
        for r in failed_reqs[:10]:
            print(f"  {r['url']}  failure={r['failure']}")

        # Persist full data
        (OUT / "diag.json").write_text(
            json.dumps({
                "title": title,
                "h1": h1,
                "auth_check": auth_check,
                "api_reqs": api_reqs,
                "api_resps": api_resps,
                "console_logs": console_logs,
                "page_errors": page_errors,
                "failed_reqs": failed_reqs,
            }, ensure_ascii=False, indent=2),
            encoding="utf-8",
        )
        print(f"\n[+] Full diagnostic data: {OUT / 'diag.json'}")

        # R59.4 (2026-09-03): hard fail on any page error so this script can
        # be used in CI / pre-commit. Previously we only logged page_errors,
        # which let the R57.3 leftover call (`setupLogsTabNavigation is not
        # defined`) ship undetected — the R59.3 round fixed WebSocket +
        # monitor auth but missed this because the smoke test only asserted
        # connectionStatus + 200/200 API responses, not init() success.
        if page_errors:
            print()
            print("=" * 60)
            print(f"[FAIL] {len(page_errors)} uncaught page error(s) — WebUI init() is broken")
            for e in page_errors:
                print(f"  - {e[:200]}")
            await browser.close()
            sys.exit(1)

        if not api_resps or any(r['status'] >= 500 for r in api_resps):
            print()
            print("=" * 60)
            print(f"[FAIL] no API responses or 5xx seen — backend not reachable")
            await browser.close()
            sys.exit(1)

        await browser.close()


asyncio.run(main())
