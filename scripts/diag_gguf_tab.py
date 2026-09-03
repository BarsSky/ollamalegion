"""Click a GGUF tab and observe what happens."""
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
        page.on("pageerror", lambda e: page_errors.append(str(e)))
        page.on("console", lambda m: console_logs.append({"type": m.type, "text": m.text[:300]}))

        await page.goto("http://localhost:18083/index.html", wait_until="domcontentloaded", timeout=15000)
        await asyncio.sleep(3)

        # Switch to GGUF
        await page.evaluate("""
            () => {
                const el = document.querySelector(".nav-item[data-page='gguf']");
                if (el) el.click();
            }
        """)
        await asyncio.sleep(4)

        # Click first backend
        await page.evaluate("""
            () => {
                const item = document.querySelector('.gguf-backend-item');
                if (item) item.click();
            }
        """)
        await asyncio.sleep(3)

        # Snapshot tabs
        tabs_before = await page.evaluate("""
            () => {
                const tabsContainer = document.querySelector('.gguf-detail-tabs');
                const tabBtns = document.querySelectorAll('.gguf-detail-tabs .gguf-tab, .gguf-tab');
                const allTabs = document.querySelectorAll('[class*="gguf-tab"]');
                return {
                    tabsContainer: tabsContainer ? tabsContainer.outerHTML.slice(0, 800) : null,
                    tabBtns_count: tabBtns.length,
                    tabBtns_sample: Array.from(tabBtns).slice(0, 5).map(t => ({
                        tag: t.tagName,
                        cls: t.className.slice(0, 100),
                        dataset: JSON.stringify(t.dataset),
                        text: t.textContent.trim().slice(0, 50)
                    })),
                    allTabsCount: allTabs.length,
                    detailContent: document.getElementById('ggufDetailContent')?.outerHTML?.slice(0, 500)
                };
            }
        """)
        print("=== Tabs found ===")
        print(json.dumps(tabs_before, indent=2, ensure_ascii=False))

        # Check current state.detailPane
        state_before = await page.evaluate("""
            () => ({
                detailPane: window.GgufModule?.state?.detailPane,
                activeTab: window.GgufModule?.state?.activeTab,
            })
        """)
        print()
        print("=== State before click ===")
        print(json.dumps(state_before, indent=2, ensure_ascii=False))

        # Try to click on "Models" tab
        click_result = await page.evaluate("""
            () => {
                const tabBtns = document.querySelectorAll('.gguf-detail-tab');
                for (const t of tabBtns) {
                    if (t.textContent.includes('Модели') || t.textContent.includes('Models')) {
                        t.click();
                        return {clicked: true, text: t.textContent.trim().slice(0, 50), cls: t.className, dataset: JSON.stringify(t.dataset)};
                    }
                }
                return {clicked: false, total: tabBtns.length};
            }
        """)
        print()
        print(f"=== Click result: {click_result} ===")

        await asyncio.sleep(2)

        # Snapshot after
        state_after = await page.evaluate("""
            () => ({
                detailPane: window.GgufModule?.state?.detailPane,
                activeTab: window.GgufModule?.state?.activeTab,
                detailPaneText: document.getElementById('ggufDetailContent')?.textContent?.slice(0, 200),
                activeTabInDOM: document.querySelector('.gguf-tab.active, [data-tab].active, [class*="tab"].active')?.textContent?.trim()?.slice(0, 50)
            })
        """)
        print()
        print("=== State after click ===")
        print(json.dumps(state_after, indent=2, ensure_ascii=False))

        print()
        print(f"=== page_errors ({len(page_errors)}) ===")
        for e in page_errors[:5]:
            print(f"  {e[:400]}")

        print()
        print(f"=== console (errors/warnings) ===")
        for c in console_logs:
            if c['type'] in ('error', 'warning'):
                print(f"  [{c['type']}] {c['text'][:300]}")

        await page.screenshot(path="C:/Ollama/ollamalegion/_diag/gguf-tab.png", full_page=True)
        print()
        print("screenshot: C:/Ollama/ollamalegion/_diag/gguf-tab.png")

        await browser.close()


asyncio.run(main())
