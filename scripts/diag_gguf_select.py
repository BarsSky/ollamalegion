"""Click a backend on GGUF page and observe what happens."""
import asyncio
import json
from playwright.async_api import async_playwright


async def main():
    async with async_playwright() as p:
        browser = await p.chromium.launch(headless=True)
        ctx = await browser.new_context(viewport={"width": 1400, "height": 900})
        page = await ctx.new_page()

        page_errors = []
        console_logs = []
        api_401 = []

        page.on("pageerror", lambda e: page_errors.append(str(e)))
        page.on("console", lambda m: console_logs.append({"type": m.type, "text": m.text[:300]}))
        def on_response(r):
            if "/api/" in r.url and r.status == 401:
                api_401.append(r.url)
        page.on("response", on_response)

        await page.goto("http://localhost:18083/index.html", wait_until="domcontentloaded", timeout=15000)
        await asyncio.sleep(3)

        # Switch to GGUF
        await page.evaluate("""
            () => {
                const el = document.querySelector(".nav-item[data-page='gguf']");
                if (el) el.click();
            }
        """)
        await asyncio.sleep(4)  # let refreshBackends complete

        # Snapshot before click
        before = await page.evaluate("""
            () => ({
                backends: (window.GgufModule?.state?.registeredBackends || []).map(b => b.id),
                selectedBackendId: window.GgufModule?.state?.selectedBackendId,
                detailPanelText: document.getElementById('ggufDetailPanel')?.textContent?.slice(0, 200),
                firstItemText: document.querySelector('.gguf-backend-item')?.textContent?.slice(0, 100),
            })
        """)
        print("=== BEFORE click ===")
        print(json.dumps(before, indent=2, ensure_ascii=False))

        # Click first backend item
        click_result = await page.evaluate("""
            () => {
                const item = document.querySelector('.gguf-backend-item');
                if (!item) return {clicked: false, reason: 'no .gguf-backend-item found'};
                const id = item.getAttribute('data-backend-id');
                item.click();
                return {clicked: true, id: id};
            }
        """)
        print()
        print(f"=== Click: {click_result} ===")

        await asyncio.sleep(4)  # let refreshDetail + render

        # Snapshot after
        after = await page.evaluate("""
            () => ({
                selectedBackendId: window.GgufModule?.state?.selectedBackendId,
                detailPanelText: document.getElementById('ggufDetailPanel')?.textContent?.slice(0, 400),
                detailPanelHTML_length: document.getElementById('ggufDetailPanel')?.innerHTML?.length,
                workerInfo: window.GgufModule?.state?.workerInfo ? 'present' : 'null',
                gpuInfo: window.GgufModule?.state?.gpuInfo ? 'present' : 'null',
            })
        """)
        print()
        print("=== AFTER click ===")
        print(json.dumps(after, indent=2, ensure_ascii=False))

        print()
        print(f"=== page_errors ({len(page_errors)}) ===")
        for e in page_errors[:5]:
            print(f"  {e[:400]}")

        print()
        print(f"=== console errors/warnings ({len([c for c in console_logs if c['type'] in ('error','warning')])}) ===")
        for c in console_logs:
            if c['type'] in ('error', 'warning'):
                print(f"  [{c['type']}] {c['text'][:300]}")

        print()
        print(f"=== API 401 ({len(api_401)}) ===")
        for u in api_401[:5]:
            print(f"  {u}")

        await page.screenshot(path="C:/Ollama/ollamalegion/_diag/gguf-selected.png", full_page=True)
        print()
        print("screenshot: C:/Ollama/ollamalegion/_diag/gguf-selected.png")

        await browser.close()


asyncio.run(main())
