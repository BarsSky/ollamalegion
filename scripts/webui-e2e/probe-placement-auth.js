// probe-placement-auth.js — почему POST /api/v1/placement/plan из страницы даёт 401.
const { chromium } = require('playwright');

(async () => {
    const url = process.argv[2] || 'http://127.0.0.1:18083';
    const browser = await chromium.launch({ channel: 'msedge', headless: true });
    const page = await browser.newPage();
    const seen = [];
    page.on('request', r => {
        if (r.url().includes('/api/v1/placement/plan')) {
            seen.push({
                method: r.method(),
                headers: r.headers(),
                body: (r.postData() || '').slice(0, 120)
            });
        }
    });
    page.on('response', async r => {
        if (r.url().includes('/api/v1/placement/plan')) {
            console.log('RESPONSE ' + r.status() + ' ' + r.url());
        }
    });
    await page.goto(url + '/index.html', { waitUntil: 'domcontentloaded' });
    await page.waitForTimeout(3000);

    const env = await page.evaluate(() => ({
        hasApi: !!(window.Api && window.Api.getAuthHeaders),
        headers: (window.Api && window.Api.getAuthHeaders) ? window.Api.getAuthHeaders() : null,
        token: (window.WEBUI_CONFIG && window.WEBUI_CONFIG.API_TOKEN) ? 'set' : 'empty'
    }));
    console.log('ENV: ' + JSON.stringify(env));

    const direct = await page.evaluate(async () => {
        const res = await fetch('/api/v1/placement/plan', {
            method: 'POST',
            headers: window.Api.getAuthHeaders(),
            body: JSON.stringify({ models: [], nodes: [] })
        });
        const text = await res.text();
        return { status: res.status, body: text.slice(0, 160) };
    });
    console.log('DIRECT fetch с Api.getAuthHeaders: ' + JSON.stringify(direct));

    await page.waitForTimeout(1500);
    console.log('запросы страницы к /placement/plan:', seen.length);
    seen.slice(0, 4).forEach(s => console.log('  ' + s.method +
        ' token=' + (s.headers['x-api-token'] ? 'да' : 'НЕТ') + ' body=' + s.body));
    await browser.close();
})();
