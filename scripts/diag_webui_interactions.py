"""Interaction-based smoke test: click primary elements per page and verify state.

User pain (R59.7-R59.9): page-coverage tests in scripts/diag_webui_pages.py
only check INITIAL RENDER — they don't click on anything. So bugs that
only surface on user interaction (tab switches, button clicks, item
selection) silently shipped.

This script catches that class of bug by DRIVING interactions and
asserting state transitions + DOM updates after each click.

Usage:
  python scripts/diag_webui_interactions.py                # localhost:18083 default

Exit 0 = all interactions OK, exit 1 = at least one interaction failed.
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


async def gguf_select_backend(page):
    """Click first backend item, assert panel re-renders."""
    item = page.locator(".gguf-backend-item").first
    if await item.count() == 0:
        return False, "no .gguf-backend-item"
    backend_id = await item.get_attribute("data-backend-id")
    await item.click()
    await page.wait_for_timeout(2500)
    state = await page.evaluate("""() => ({
        selected: window.GgufModule?.state?.selectedBackendId,
        panelHTMLLen: document.getElementById('ggufDetailPanel')?.innerHTML?.length || 0,
        detailContentId: !!document.getElementById('ggufDetailContent'),
    })""")
    if state["selected"] != backend_id:
        return False, f"selectedBackendId={state['selected']!r} expected {backend_id!r}"
    if state["panelHTMLLen"] < 1500:
        return False, f"panel HTML length={state['panelHTMLLen']} (expected >= 1500)"
    if not state["detailContentId"]:
        return False, "#ggufDetailContent id missing (R59.9 fix missing?)"
    return True, f"selected={state['selected']}, panel={state['panelHTMLLen']}b"


async def gguf_switch_models_tab(page):
    """Click Models tab, assert state + active class moved."""
    tabs = page.locator(".gguf-detail-tab")
    count = await tabs.count()
    target = None
    for i in range(count):
        text = (await tabs.nth(i).text_content() or "").strip()
        if "Models" in text or "Модели" in text:
            target = tabs.nth(i)
            tab_id = await target.get_attribute("data-detail-tab")
            break
    if target is None:
        return False, f"no Models/Модели tab found ({count} tabs)"
    await target.click()
    await page.wait_for_timeout(500)
    state = await page.evaluate("""() => ({
        detailPane: window.GgufModule?.state?.detailPane,
        activeTabInDOM: document.querySelector('.gguf-detail-tab.active')?.getAttribute('data-detail-tab'),
        detailPaneText: document.getElementById('ggufDetailContent')?.textContent?.slice(0, 80),
    })""")
    if state["detailPane"] != tab_id:
        return False, f"state.detailPane={state['detailPane']!r} expected {tab_id!r}"
    if state["activeTabInDOM"] != tab_id:
        return False, f"DOM active tab={state['activeTabInDOM']!r} expected {tab_id!r} (tab row not re-rendered)"
    return True, f"detailPane={state['detailPane']}, active={state['activeTabInDOM']}, content={state['detailPaneText']!r}"


# Map: page_name -> {steps: [(label, async fn(page)), ...], wait_after_nav_ms: int}
INTERACTIONS = {
    "dashboard": {"steps": [], "wait_after_nav_ms": 2000},
    "monitor":   {"steps": [], "wait_after_nav_ms": 2000},
    "backends":  {"steps": [], "wait_after_nav_ms": 2000},
    "models":    {"steps": [], "wait_after_nav_ms": 2000},
    "sessions":  {"steps": [], "wait_after_nav_ms": 2000},
    "queue":     {"steps": [], "wait_after_nav_ms": 2000},
    "gguf": {
        "steps": [
            ("select backend", gguf_select_backend),
            ("switch to Models tab", gguf_switch_models_tab),
        ],
        "wait_after_nav_ms": 4000,  # backends fetch
    },
    "agents":    {"steps": [], "wait_after_nav_ms": 2000},
    "logs":      {"steps": [], "wait_after_nav_ms": 2000},
    "settings":  {"steps": [], "wait_after_nav_ms": 2000},
}


async def main():
    failed = []
    summary = {}

    async with async_playwright() as p:
        browser = await p.chromium.launch(headless=True)
        ctx = await browser.new_context(viewport={"width": 1400, "height": 900})
        page = await ctx.new_page()

        page_errors = []
        api_401_count = [0]

        def on_response(r):
            if "/api/" in r.url and r.status == 401:
                api_401_count[0] += 1
        page.on("response", on_response)
        page.on("pageerror", lambda e: page_errors.append(str(e)))

        try:
            await page.goto(URL, wait_until="domcontentloaded", timeout=15000)
        except Exception as e:
            print(f"[FAIL] cannot open {URL}: {e}")
            await browser.close()
            sys.exit(1)

        await page.wait_for_function(
            "() => window.App && window.GgufModule && window.GgufRenderer",
            timeout=10000,
        )
        await asyncio.sleep(2)

        cs = await page.evaluate("() => document.getElementById('connectionStatus')?.textContent?.trim()")
        print(f"[i] initial connectionStatus: {cs}")

        nav_items = await page.evaluate("""
            () => Array.from(document.querySelectorAll('.nav-item[data-page]'))
                .filter(el => (el.getAttribute('href') || '').startsWith('#'))
                .map(el => el.getAttribute('data-page'))
        """)
        print(f"[i] in-page nav-items: {nav_items}")

        for name in nav_items:
            page_errors.clear()
            api_401_count[0] = 0
            cfg = INTERACTIONS.get(name, {"steps": [], "wait_after_nav_ms": 2000})
            print()
            print(f"--- Page: {name} ---")

            try:
                await page.evaluate("""
                    (pageName) => {
                        const el = document.querySelector(`.nav-item[data-page='${pageName}']`);
                        if (el) el.click();
                    }
                """, name)
            except Exception as e:
                print(f"  [FAIL] cannot switch to {name}: {e}")
                failed.append((name, "switch failed", str(e)))
                continue

            await asyncio.sleep(cfg["wait_after_nav_ms"] / 1000)

            step_results = []
            for label, fn in cfg["steps"]:
                try:
                    ok, detail = await fn(page)
                except Exception as e:
                    ok, detail = False, f"exception: {e}"
                status = "[OK]" if ok else "[FAIL]"
                print(f"  {status} {label}: {detail}")
                step_results.append({"step": label, "ok": ok, "detail": detail})
                if not ok:
                    failed.append((name, label, detail))
                    break

            summary[name] = {
                "page_errors": len(page_errors),
                "api_401": api_401_count[0],
                "steps": step_results,
            }
            if page_errors:
                for e in page_errors[:3]:
                    print(f"  PAGE_ERROR: {e[:200]}")

        print()
        print("=" * 60)
        print("=== SUMMARY ===")
        for name, s in summary.items():
            steps_str = ", ".join(
                f"{'OK' if st['ok'] else 'FAIL'}/{st['step']}" for st in s["steps"]
            ) or "(no interactions tested)"
            err = f", errors={s['page_errors']}, 401={s['api_401']}" if s["page_errors"] or s["api_401"] else ""
            print(f"  {name:10} {steps_str}{err}")

        (OUT / "diag_interactions.json").write_text(
            json.dumps(summary, ensure_ascii=False, indent=2),
            encoding="utf-8",
        )
        print()
        print(f"[+] Full report: {OUT / 'diag_interactions.json'}")

        await browser.close()

    if failed:
        print()
        print("=" * 60)
        print(f"[FAIL] {len(failed)} interaction(s) failed:")
        for name, step, detail in failed:
            print(f"  - {name}/{step}: {detail}")
        sys.exit(1)

    print()
    print("=" * 60)
    print("[PASS] all interactions OK")
    sys.exit(0)


asyncio.run(main())
