// debug-diag-provider-r493.js — точечная проверка: почему провайдер диагностики
// не даёт запросов. Запускается вручную, в репозитории остаётся как инструмент.
'use strict';

const { chromium } = require('C:/Ollama/ollamalegion/node_modules/playwright');

const BASE = process.argv[2] || 'http://127.0.0.1:18085';

function loginRequest(base, user, pass) {
    return new Promise((resolve, reject) => {
        const body = JSON.stringify({ name: user, password: pass });
        const url = new URL(base + '/api/v1/auth/login');
        const req = require('http').request({
            hostname: url.hostname, port: url.port || 80, path: url.pathname, method: 'POST',
            headers: { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) },
        }, (res) => {
            let data = '';
            res.on('data', (c) => { data += c; });
            res.on('end', () => {
                const raw = res.headers['set-cookie'] || [];
                const line = raw.find((c) => c.indexOf('ol_session=') === 0) || '';
                const m = line.match(/ol_session=([^;]+)/);
                resolve({ cookie: m ? m[1] : '', body: data.slice(0, 200) });
            });
        });
        req.on('error', reject);
        req.write(body);
        req.end();
    });
}

(async () => {
    const browser = await chromium.launch({ channel: 'msedge' });
    const ctx = await browser.newContext();
    const login = await loginRequest(BASE, process.env.VERIFY_ADMIN_USER, process.env.VERIFY_ADMIN_PASSWORD);
    console.log('cookie получена:', !!login.cookie, login.body);
    await ctx.addCookies([{ name: 'ol_session', value: login.cookie, domain: new URL(BASE).hostname, path: '/', httpOnly: true, sameSite: 'Lax' }]);

    const page = await ctx.newPage();
    const log = [];
    page.on('response', (r) => {
        const u = r.url();
        if (u.indexOf('/api/') !== -1) log.push(r.status() + ' ' + u.replace(BASE, ''));
    });

    await page.goto(BASE + '/?page=diagnostics', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#dataFreshness', { timeout: 20000 });
    await page.waitForTimeout(3000);

    const info = await page.evaluate(() => {
        const DR = window.DataRefresh;
        const provs = DR && DR._providers ? Object.keys(DR._providers) : [];
        const diagProvs = (DR && DR._providers && DR._providers.diagnostics) ? DR._providers.diagnostics.length : 0;
        return {
            providers: provs,
            diagCount: diagProvs,
            hasProviderApi: !!(DR && typeof DR.hasProvider === 'function'),
            hasProviderDiag: !!(DR && typeof DR.hasProvider === 'function' && DR.hasProvider('diagnostics')),
            currentPage: (window.App && window.App.context) ? window.App.context.currentPage : '(нет контекста)',
            isAuto: DR ? DR.isAuto() : null,
            lastError: DR ? DR._state.lastError : null,
            apiDiagSummary: !!(window.Api && typeof window.Api.diagSummary === 'function'),
        };
    });
    console.log('состояние страницы:', JSON.stringify(info, null, 2));
    console.log('первые запросы:', log.slice(0, 12));

    const before = log.length;
    await page.waitForTimeout(11000);
    const during = log.slice(before);
    console.log('запросы за 11 с:', during.length, during);

    // Ручной вызов провайдера — видно, что именно он делает.
    const manual = await page.evaluate(async () => {
        const DR = window.DataRefresh;
        if (!DR || !DR._providers || !DR._providers.diagnostics) return 'провайдера нет';
        try {
            const t0 = Date.now();
            const res = await Promise.all(DR._providers.diagnostics.map((f) => f()));
            return { ok: true, results: res, ms: Date.now() - t0 };
        } catch (e) { return { ok: false, error: String(e && e.message || e) }; }
    });
    console.log('ручной вызов провайдера:', JSON.stringify(manual, null, 2));

    await browser.close();
})().catch((e) => { console.error('СБОЙ: ' + (e && e.stack || e)); process.exit(2); });
