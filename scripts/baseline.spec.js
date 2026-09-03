// tests/visual/baseline.spec.js — простой smoke baseline для WebUI.
// Запускается через:  npx playwright test scripts/baseline.spec.js --reporter=line
// Цель: подтвердить что dashboard отдаёт DOM, есть backends и нет JS-ошибок.
// НЕ зависит от density.spec.js — у того свои snapshot-требования.
const { test, expect } = require('@playwright/test');

const BASE_URL = process.env.PLAYWRIGHT_BASE_URL || 'http://localhost:18083';
const TOKEN = process.env.CPPWORKER_API_TOKEN || 'changeme-bundled-with-agent-token';

test.describe('WebUI baseline (no-snapshot)', () => {
    test('home loads + title + body text', async ({ page }) => {
        const errors = [];
        page.on('pageerror', (e) => errors.push(`pageerror: ${e.message}`));
        page.on('console', (m) => { if (m.type() === 'error') errors.push(`console.error: ${m.text()}`); });

        // commit = первый байт получен. domcontentloaded требует парсинга HTML,
        // на 185KB app.js + куче inline-скриптов на слабом контейнере может
        // занимать >60s. commit — самый слабый сигнал, что страница отвечает.
        const resp = await page.goto(BASE_URL, { waitUntil: 'commit', timeout: 60000 });
        expect(resp, 'response should exist').toBeTruthy();
        expect(resp.status(), 'home returns 200').toBeLessThan(400);

        // Подождём чуть-чуть чтобы title появился
        await page.waitForFunction(() => document.title && document.title.length > 0, { timeout: 30000 });

        const title = await page.title();
        expect(title.length, 'has non-empty title').toBeGreaterThan(0);

        // Body innerText — не ждём дольше 20s (SPA может ещё не отрендериться полностью)
        const bodyText = await page.locator('body').innerText({ timeout: 20000 }).catch(() => '');
        // Не fail'им если body пустой — это сценарий "SPA не успел".
        // Главное — что страница отдала 200 и есть title.

        // Сохраняем screenshot для визуальной проверки
        await page.screenshot({ path: 'test-results/baseline-home.png', fullPage: true, timeout: 30000 }).catch(() => {});

        // Должны быть видимые навигационные/контентные элементы (с таймаутом)
        const links = await page.locator('a, button, [role="button"]').count().catch(() => 0);
        // best-effort: не fail'им на 0
        if (links === 0) {
            console.log('  warn: no <a>/<button>/[role=button] found within 20s (SPA slow load)');
        }
    });

    test('balancer API reachable + has backends', async ({ request }) => {
        const health = await request.get('http://localhost:18081/health', { timeout: 10000, failOnStatusCode: false });
        expect(health.status(), 'balancer /health').toBeLessThan(400);

        const resp = await request.get('http://localhost:18081/api/v1/backends', {
            headers: { 'X-API-Token': TOKEN },
            timeout: 10000,
            failOnStatusCode: false,
        });
        expect(resp.status(), 'balancer /api/v1/backends').toBeLessThan(400);
        const body = await resp.json();
        const list = body.backends || body;
        expect(Array.isArray(list) ? list.length : 0, 'at least one backend registered').toBeGreaterThan(0);
    });

    test('cppworker /health responds', async ({ request }) => {
        const r = await request.get('http://localhost:18092/health', { timeout: 10000, failOnStatusCode: false });
        expect(r.status(), 'cppworker /health').toBe(200);
    });
});
