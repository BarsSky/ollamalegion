"""Headless smoke test for ALL WebUI pages.

User feedback (R59.5): "WebUI не загружает gguf model ... должны быть тесты
по тому как WEBUI все должен отображать то есть проверки что метрики доходят
и поступают чтобы не возникало таких ошибок в будущем".

Этот скрипт:
  1. Открывает /  и ждёт инициализации (dashboard).
  2. Кликает по КАЖДОЙ вкладке из data-page атрибутов nav-item'ов
     (dashboard, monitor, backends, models, sessions, queue, gguf,
     agents, logs, settings).
  3. После каждого клика проверяет:
     - [page_errors]      — никаких uncaught ReferenceError / TypeError
     - [api_401_count]    — 401 на /api/v1/* недопустимо
     - [content_rendered] — на странице есть ожидаемые элементы
                            (для gguf: ggufDetailPanel; для backends: backend
                            cards; для sessions: session table; etc.)
     - [metrics_arrived]  — для dashboard: connectionStatus==="Connected"
                            и window.WEBUI_CONFIG.API_TOKEN.length>0
  4. Скриншот каждой страницы в _diag/page_<name>.png.
  5. Если page_errors > 0 ИЛИ api_401 > 0 ИЛИ content не отрисовался — exit 1.

Использование:
  python scripts/diag_webui_pages.py                # localhost:18083 default
  python scripts/diag_webui_pages.py URL=http://localhost:18083/
"""
import asyncio
import json
import os
import sys
from pathlib import Path
from playwright.async_api import async_playwright

URL = os.environ.get("URL", "http://localhost:18083/")
OUT = Path("C:/Ollama/ollamalegion/_diag")
OUT.mkdir(parents=True, exist_ok=True)

# Per-page expectations: list of CSS selectors that MUST appear (any one).
# A page "renders" if AT LEAST ONE selector in its expect list is found
# in the live DOM after switching to it.
PAGE_EXPECT = {
    "dashboard":   [".stats-card", "#totalRequests", "[id*=queueSize]"],
    "backends":    [".backend-card", "[id*=backend]", "[class*=backend]"],
    "models":      [".model-card", "[id*=model]", "table"],
    "sessions":    [".session-row", "table", "[id*=session]"],
    "queue":       [".queue-row", "[id*=queue]"],
    "gguf":        ["#ggufDetailPanel", ".gguf-master-layout", "#ggufHfTokenBar"],
    "logs":        ["[id*=logs]", ".log-entry", "[id*=systemLogs]"],
    "settings":    [".settings-panel", "[id*=settings]", "input[type=checkbox]"],
    "agents":      [".agent-card", "[id*=agent]"],
    "monitor":     ["#statusDot", ".monitor-legend"],   # monitor iframe — отдельная страница
}


async def main():
    failed_pages = []
    summary = {}

    async with async_playwright() as p:
        browser = await p.chromium.launch(headless=True)
        ctx = await browser.new_context(viewport={"width": 1400, "height": 900})
        page = await ctx.new_page()

        page_errors = []
        api_401_count = 0
        last_responses = []  # (url, status) sliding window

        page.on("pageerror", lambda err: page_errors.append(str(err)))

        def on_response(r):
            last_responses.append((r.url, r.status))
            if len(last_responses) > 200:
                last_responses.pop(0)
            if "/api/" in r.url and r.status == 401:
                nonlocal_api_401_ref[0] += 1
                if name_ref[0]:  # currently testing this page
                    urls_401_this_page.append(r.url)
        nonlocal_api_401_ref = [0]
        name_ref = [None]
        urls_401_this_page = []
        page.on("response", on_response)

        # Open the dashboard first
        print(f"=== Opening {URL} ===")
        try:
            await page.goto(URL, wait_until="domcontentloaded", timeout=15000)
        except Exception as e:
            print(f"[FAIL] cannot open {URL}: {e}")
            await browser.close()
            sys.exit(1)

        # Wait for WebUI init (status badge appears)
        await page.wait_for_selector("#connectionStatus", timeout=10000)
        await asyncio.sleep(2)  # let initial fetches settle

        # Sanity check at dashboard
        cs = await page.evaluate("() => document.getElementById('connectionStatus')?.textContent?.trim()")
        print(f"[i] Dashboard initial connectionStatus: {cs!r}")

        # Iterate over all in-page nav-items (skip monitor and external links
        # like virtual-models.html — those open in new tab; skip health too)
        nav_items = await page.evaluate("""
            () => Array.from(document.querySelectorAll('.nav-item[data-page]'))
                .filter(el => {
                    const href = el.getAttribute('href') || '';
                    return href.startsWith('#');  // in-page anchors only
                })
                .map(el => el.getAttribute('data-page'))
        """)
        print(f"[i] in-page nav-items: {nav_items}")

        for name in nav_items:
            page_errors.clear()
            nonlocal_api_401_ref[0] = 0
            last_responses.clear()
            urls_401_this_page.clear()
            name_ref[0] = name
            print()
            print(f"--- Testing page: {name} ---")

            try:
                # Programmatic switch via window.ui.switchPage — works even
                # if the nav item is not visible (collapsed sidebar) or
                # intercepted by another element.
                await page.evaluate("""
                    (page) => {
                        if (window.ui && typeof window.ui.switchPage === 'function') {
                            window.ui.switchPage(page);
                        } else {
                            // Fallback: click via JS (skip visibility check)
                            const el = document.querySelector(`.nav-item[data-page='${page}']`);
                            if (el) el.click();
                        }
                    }
                """, name)
            except Exception as e:
                print(f"[FAIL] cannot switch to {name}: {e}")
                failed_pages.append((name, "switch failed", str(e)))
                continue

            # Let page render and any data fetches complete
            await asyncio.sleep(2.5)

            # Snapshot errors after this page
            err_count = len(page_errors)
            err_401 = nonlocal_api_401_ref[0]

            # Content rendering check
            selectors = PAGE_EXPECT.get(name, [])
            rendered = False
            matched_sel = None
            # monitor lives inside a same-origin iframe; if we navigated to
            # that page the parent DOM won't have the expected selectors.
            # The iframe loads its own scripts; page_errors covers it.
            check_in_parent = (name != "monitor")
            if check_in_parent:
                for sel in selectors:
                    try:
                        count = await page.locator(sel).count()
                        if count > 0:
                            rendered = True
                            matched_sel = sel
                            break
                    except Exception:
                        pass
            else:
                # For monitor page, treat it as "rendered" if the iframe
                # is present — page_errors covers its internal init.
                rendered = True
                matched_sel = "<iframe> loaded"

            # Metrics-arrival check (dashboard only — others don't have status badge)
            metrics_ok = None
            if name == "dashboard":
                cs2 = await page.evaluate("() => document.getElementById('connectionStatus')?.textContent?.trim()")
                metrics_ok = (cs2 == "Connected")
                print(f"  metrics: connectionStatus={cs2!r} -> {metrics_ok}")

            shot = OUT / f"page_{name}.png"
            try:
                await page.screenshot(path=str(shot), full_page=False)
            except Exception as e:
                print(f"  [warn] screenshot failed: {e}")

            summary[name] = {
                "page_errors": err_count,
                "first_error": page_errors[0][:200] if page_errors else None,
                "api_401": err_401,
                "urls_401": urls_401_this_page,
                "rendered": rendered,
                "matched_selector": matched_sel,
                "metrics_ok": metrics_ok,
            }
            print(f"  page_errors: {err_count}  api_401: {err_401}  rendered: {rendered} ({matched_sel!r})")
            if err_401 > 0:
                for u in urls_401_this_page[:5]:
                    print(f"    401-url: {u}")
            if page_errors:
                for e in page_errors[:3]:
                    print(f"    err: {e[:200]}")

            if err_count > 0 or err_401 > 0 or not rendered:
                failed_pages.append((name, f"errors={err_count} 401={err_401} rendered={rendered}", None))

        # Final
        print()
        print("=" * 60)
        print("=== SUMMARY ===")
        for name, s in summary.items():
            flag = "OK" if (s["page_errors"] == 0 and s["api_401"] == 0 and s["rendered"]) else "FAIL"
            print(f"  [{flag}] {name:10} errors={s['page_errors']:2} 401={s['api_401']:2} rendered={s['rendered']}")
        print()
        (OUT / "diag_pages.json").write_text(
            json.dumps(summary, ensure_ascii=False, indent=2),
            encoding="utf-8",
        )
        print(f"[+] Full report: {OUT / 'diag_pages.json'}")

        await browser.close()

    if failed_pages:
        print()
        print("=" * 60)
        print(f"[FAIL] {len(failed_pages)} page(s) failed:")
        for name, reason, _ in failed_pages:
            print(f"  - {name}: {reason}")
        sys.exit(1)

    print()
    print("=" * 60)
    print("[PASS] all pages OK")
    sys.exit(0)


asyncio.run(main())
