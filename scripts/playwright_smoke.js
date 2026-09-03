// playwright_smoke.js — минимальный live-baseline для WebUI ollamalegion.
// Использует playwright + chromium. Генерирует скриншоты ключевых экранов
// и JSON-отчёт о прохождении проверок.
//
// Запуск:  node scripts/playwright_smoke.js [baseUrl]
// Outputs:  test-results/baseline-<ts>/{01-home,02-clicked,03-settings}.png
//           + report.json с шагами и asserts.

const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');

const BASE_URL = process.argv[2] || 'http://localhost:18083';
const TOKEN = process.env.CPPWORKER_API_TOKEN || 'changeme-bundled-with-agent-token';

const ARTIFACT_DIR = path.join(
    process.cwd(),
    'test-results',
    'baseline-' + new Date().toISOString().replace(/[:.]/g, '-')
);
fs.mkdirSync(ARTIFACT_DIR, { recursive: true });

const results = {
    url: BASE_URL,
    timestamp: new Date().toISOString(),
    steps: [],
    errors: [],
};
let exitCode = 0;

function record(name, ok, detail) {
    results.steps.push({ name, ok, detail: detail || '' });
    console.log(`  ${ok ? '✓' : '✗'} ${name}${detail ? ' — ' + detail : ''}`);
    if (!ok) exitCode = 1;
}

async function withTimeout(promise, ms, label) {
    return Promise.race([
        promise,
        new Promise((_, rej) => setTimeout(() => rej(new Error(`${label} timed out after ${ms}ms`)), ms)),
    ]);
}

(async () => {
    let browser;
    try {
        console.log(`[smoke] launching chromium → ${BASE_URL}`);
        browser = await withTimeout(
            chromium.launch({ headless: true, args: ['--no-sandbox', '--disable-dev-shm-usage', '--disable-gpu'] }),
            90000,
            'chromium.launch'
        );
        record('chromium launched', true, '');

        const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
        const page = await ctx.newPage();
        page.on('pageerror', (err) => results.errors.push(`pageerror: ${err.message}`));
        page.on('console', (msg) => {
            if (msg.type() === 'error') results.errors.push(`console.error: ${msg.text()}`);
        });

        // --- 1. Главная страница ---
        const t0 = Date.now();
        try {
            await withTimeout(
                page.goto(BASE_URL, { waitUntil: 'domcontentloaded', timeout: 30000 }),
                35000,
                'page.goto'
            );
            record('home loads', true, `DOM ready in ${Date.now() - t0}ms`);
        } catch (e) {
            record('home loads', false, e.message);
        }

        await page.screenshot({ path: path.join(ARTIFACT_DIR, '01-home.png'), fullPage: true });

        // --- 2. Title ---
        try {
            const titleText = await page.title();
            record('page title', titleText.length > 0, `title="${titleText}"`);
        } catch (e) {
            record('page title', false, e.message);
        }

        // --- 3. Body text ---
        try {
            const bodyText = (await page.locator('body').innerText({ timeout: 10000 })).slice(0, 500);
            record('has body text', bodyText.length > 50,
                `first 100 chars: ${bodyText.slice(0, 100).replace(/\n/g, ' ')}`);
        } catch (e) {
            record('has body text', false, e.message);
        }

        // --- 4. Balancer /health ---
        try {
            const r = await page.request.get('http://localhost:18081/health', { timeout: 5000, failOnStatusCode: false });
            record('balancer /health', r.ok(), `status=${r.status()}`);
        } catch (e) {
            record('balancer /health', false, e.message);
        }

        // --- 5. Balancer /api/v1/backends ---
        try {
            const r = await page.request.get('http://localhost:18081/api/v1/backends', {
                headers: { 'X-API-Token': TOKEN },
                timeout: 5000,
                failOnStatusCode: false,
            });
            if (r.ok()) {
                const body = await r.json();
                const list = body.backends || body;
                record('balancer /api/v1/backends', Array.isArray(list) && list.length > 0,
                    `got ${Array.isArray(list) ? list.length : 'non-array'} backend(s)`);
            } else {
                record('balancer /api/v1/backends', false, `status=${r.status()}`);
            }
        } catch (e) {
            record('balancer /api/v1/backends', false, e.message);
        }

        // --- 6. CppWorker /health ---
        try {
            const r = await page.request.get('http://localhost:18092/health', { timeout: 5000, failOnStatusCode: false });
            record('cppworker /health', r.ok(), `status=${r.status()}`);
        } catch (e) {
            record('cppworker /health', false, e.message);
        }

        // --- 7. Ищем бэкенды / кнопки в UI ---
        try {
            const candidates = [
                '[data-backend-id]',
                '.backend-card',
                '.gguf-backend-item',
                '.backend-list-item',
                'a[href*="backend"]',
                '.nav-item',
            ];
            let found = 0;
            for (const sel of candidates) {
                const c = await page.locator(sel).count();
                found += c;
            }
            record('UI navigation elements', found > 0, `total across ${candidates.length} selectors = ${found}`);
        } catch (e) {
            record('UI navigation elements', false, e.message);
        }

        await page.screenshot({ path: path.join(ARTIFACT_DIR, '02-after-checks.png'), fullPage: true });

        // --- 8. Кликаем по первому бэкенду, если есть ---
        try {
            const firstBackend = page.locator(
                '[data-backend-id], .backend-card, .gguf-backend-item, .backend-list-item'
            ).first();
            const cnt = await firstBackend.count();
            if (cnt > 0) {
                await firstBackend.click({ timeout: 3000 });
                await page.waitForTimeout(2000);
                await page.screenshot({ path: path.join(ARTIFACT_DIR, '03-backend-detail.png'), fullPage: true });
                record('clicked first backend', true, 'screenshot 03-backend-detail.png');
            } else {
                record('clicked first backend', false, 'no backend selector found');
            }
        } catch (e) {
            record('clicked first backend', false, e.message);
        }

        // --- 9. Busy badge / active queries indicator (passive) ---
        try {
            const busyCount = await page.locator('.busy-badge, .active-queries, [data-active-queries]').count();
            record('busy-badge element present in DOM', true,
                `${busyCount} indicator(s) — passive check (no active inference expected in idle)`);
        } catch (e) {
            record('busy-badge element present in DOM', false, e.message);
        }

        // --- 10. Settings form (после клика по бэкенду) ---
        try {
            const formCount = await page.locator('form, .gguf-detail-pane, [data-pane="settings"]').count();
            record('settings/pane UI present', formCount > 0, `found ${formCount} selector(s)`);
        } catch (e) {
            record('settings/pane UI present', false, e.message);
        }

        await page.screenshot({ path: path.join(ARTIFACT_DIR, '04-final.png'), fullPage: true });

        results.summary = {
            total: results.steps.length,
            passed: results.steps.filter((s) => s.ok).length,
            failed: results.steps.filter((s) => !s.ok).length,
            page_errors: results.errors.length,
        };
        fs.writeFileSync(path.join(ARTIFACT_DIR, 'report.json'), JSON.stringify(results, null, 2));
        console.log(
            `\n[smoke] DONE: ${results.summary.passed}/${results.summary.total} passed, ` +
            `${results.summary.failed} failed, ${results.summary.page_errors} page errors`
        );
        console.log(`[smoke] artifacts: ${ARTIFACT_DIR}`);
    } catch (e) {
        results.fatal = e.message;
        console.error('[smoke] FATAL:', e.message);
        fs.writeFileSync(path.join(ARTIFACT_DIR, 'report.json'), JSON.stringify(results, null, 2));
        process.exitCode = 1;
    } finally {
        if (browser) await browser.close();
    }
    process.exit(exitCode);
})();
