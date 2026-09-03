"""Quick diagnostic: what does GGUF page actually show / request?
User report: "в webui еще осталась проблема с отображением существующих бэкендов gguf models - ничего нет хотя в dashboard и монитор все отображается"."""
import asyncio
import json
from playwright.async_api import async_playwright


async def main():
    async with async_playwright() as p:
        browser = await p.chromium.launch(headless=True)
        ctx = await browser.new_context(viewport={"width": 1400, "height": 900})
        page = await ctx.new_page()

        api_reqs = []
        api_resps = []
        page_errors = []
        console_logs = []

        page.on("pageerror", lambda e: page_errors.append(str(e)))
        page.on("console", lambda m: console_logs.append({"type": m.type, "text": m.text[:300]}))

        def on_request(r):
            if "/api/" in r.url:
                api_reqs.append({"url": r.url, "method": r.method, "headers": dict(r.headers)})
        def on_response(r):
            if "/api/" in r.url:
                try:
                    api_resps.append({"url": r.url, "status": r.status, "body_sample": (r.text() if False else "")[:0]})
                except Exception:
                    pass
        page.on("request", on_request)
        page.on("response", on_response)

        await page.goto("http://localhost:18083/index.html", wait_until="domcontentloaded", timeout=15000)
        # Wait for app init
        try:
            await page.wait_for_function("() => window.ui && typeof window.ui.switchPage === 'function'", timeout=10000)
        except Exception as e:
            print(f"[!] window.ui not ready: {e}")
        await asyncio.sleep(2)

        # Diagnostic dump if window.ui still not ready
        diag = await page.evaluate("""
            () => ({
                ui_defined: typeof window.ui,
                App_defined: typeof window.App,
                GgufRenderer_defined: typeof window.GgufRenderer,
                GgufModule_defined: typeof window.GgufModule,
                docReadyState: document.readyState,
            })
        """)
        print(f"=== Diag after init wait ===")
        print(json.dumps(diag, indent=2))

        # Switch to GGUF page — use click() fallback since window.ui may be undefined
        await page.evaluate("""
            () => {
                // Try switchPage first
                if (window.ui && typeof window.ui.switchPage === 'function') {
                    window.ui.switchPage('gguf');
                } else {
                    // Fallback: click nav-item (the App.setupNavigation click handler
                    // closes over switchPage in app.js and calls it)
                    const el = document.querySelector(".nav-item[data-page='gguf']");
                    if (el) el.click();
                }
            }
        """)

        # (Switched to GGUF above via fallback)
        await asyncio.sleep(4)  # let data fetches complete

        # Probe GgufModule state
        probe = await page.evaluate("""
            () => {
                const M = window.GgufModule;
                const r = {
                    M_exists: !!M,
                    M_keys: M ? Object.keys(M) : null,
                    state_keys: M && M.state ? Object.keys(M.state) : null,
                    registeredBackends_count: M && M.state ? (M.state.registeredBackends || []).length : null,
                    registeredBackends_sample: M && M.state ? (M.state.registeredBackends || []).slice(0, 3).map(b => ({
                        id: b.id, name: b.name, status: b.status, type: b.type, host: b.host, port: b.port
                    })) : null,
                    selectedBackendId: M && M.state ? M.state.selectedBackendId : null,
                    activeTab: M && M.state ? M.state.activeTab : null,
                    apiLayerExists: typeof window.Api,
                    ggufApiExists: typeof window.GgufApi,
                    ggufApiKeys: window.GgufApi ? Object.keys(window.GgufApi).slice(0, 10) : null,
                };
                return r;
            }
        """)
        print("=== GGUF Module probe ===")
        print(json.dumps(probe, indent=2, ensure_ascii=False))

        # Probe DOM
        dom = await page.evaluate("""
            () => {
                const list = document.getElementById('ggufBackendList');
                const detail = document.getElementById('ggufDetailPanel');
                const empty = document.querySelector('.gguf-empty-state');
                const cards = document.querySelectorAll('.gguf-backend-item, .gguf-backend-card, .backend-card-gguf, [class*="backend-card"]');
                return {
                    backendList_exists: !!list,
                    backendList_innerHTML_length: list ? list.innerHTML.length : 0,
                    backendList_text: list ? list.textContent.slice(0, 500) : null,
                    detailPanel_exists: !!detail,
                    detailPanel_text: detail ? detail.textContent.slice(0, 500) : null,
                    emptyStateText: empty ? empty.textContent : null,
                    backendCards_count: cards.length,
                };
            }
        """)
        print()
        print("=== DOM probe ===")
        print(json.dumps(dom, indent=2, ensure_ascii=False))

        # API requests to /gguf/
        print()
        print(f"=== {len(api_reqs)} API requests ===")
        for r in api_reqs[:30]:
            tok = r['headers'].get('x-api-token', 'MISSING')[:25]
            print(f"  {r['method']} {r['url'][:90]}  token={tok}")
        print()
        print(f"=== {len(api_resps)} API responses ===")
        for r in api_resps[:30]:
            print(f"  {r['status']} {r['url'][:90]}")

        print()
        print(f"=== page_errors ({len(page_errors)}) ===")
        for e in page_errors[:5]:
            print(f"  {e[:300]}")
        print()
        print(f"=== console (errors/warnings) ===")
        for c in console_logs:
            if c['type'] in ('error', 'warning'):
                print(f"  [{c['type']}] {c['text'][:200]}")

        await page.screenshot(path="C:/Ollama/ollamalegion/_diag/gguf-page.png", full_page=True)
        print()
        print("screenshot: C:/Ollama/ollamalegion/_diag/gguf-page.png")

        await browser.close()


asyncio.run(main())
