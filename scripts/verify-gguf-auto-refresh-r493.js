// verify-gguf-auto-refresh-r493.js — живая проверка ДОЛГОЖДАННОГО сценария:
// «модель загрузили ВНЕ открытой страницы GGUF — появляется ли она без F5».
//
// Оператор: «на странице GGUF информация по загруженным моделям на бэкендах
// появляется только после обновления страницы». Проверка воспроизводит ровно это:
//   1. страница GGUF открыта, модель не загружена;
//   2. модель грузится ВНЕ страницы (HTTP-запрос, как это сделал бы curl/другая
//      вкладка);
//   3. страница НЕ перезагружается — ждём, появится ли строка сама.
//
// Запуск: node scripts/verify-gguf-auto-refresh-r493.js http://127.0.0.1:18083
// Логин/пароль и токен — из окружения (VERIFY_ADMIN_USER/VERIFY_ADMIN_PASSWORD,
// VERIFY_API_TOKEN). Модель по умолчанию: Qwen3-4B-Instruct-2507-Q4_K_M.
'use strict';

const { chromium } = require('C:/Ollama/ollamalegion/node_modules/playwright');

const BASE = process.argv[2] || 'http://127.0.0.1:18083';
const MODEL = process.env.VERIFY_GGUF_MODEL || 'Qwen3-4B-Instruct-2507-Q4_K_M';

function httpJson(method, base, path, body, token) {
    return new Promise((resolve, reject) => {
        const payload = body ? JSON.stringify(body) : null;
        const url = new URL(base + path);
        const headers = { 'Content-Type': 'application/json' };
        if (token) headers['X-API-Token'] = token;
        if (payload) headers['Content-Length'] = Buffer.byteLength(payload);
        const req = require('http').request({
            hostname: url.hostname, port: url.port || 80, path: url.pathname + url.search,
            method, headers,
        }, (res) => {
            let data = '';
            res.on('data', (c) => { data += c; });
            res.on('end', () => {
                let parsed = null;
                try { parsed = JSON.parse(data); } catch (e) { /* не JSON */ }
                resolve({ status: res.statusCode, headers: res.headers, text: data, json: parsed });
            });
        });
        req.on('error', reject);
        if (payload) req.write(payload);
        req.end();
    });
}

async function login(base, user, pass) {
    const r = await httpJson('POST', base, '/api/v1/auth/login', { name: user, password: pass }, '');
    const raw = r.headers['set-cookie'] || [];
    const line = raw.find((c) => c.indexOf('ol_session=') === 0) || '';
    const m = line.match(/ol_session=([^;]+)/);
    return m ? m[1] : '';
}

(async () => {
    const token = process.env.VERIFY_API_TOKEN || '';
    // бэкенд берём из публичного ответа балансера
    const backends = await httpJson('GET', BASE, '/api/v1/gguf/backends', null, token);
    const list = (backends.json && (backends.json.backends || backends.json.items)) || [];
    const backend = list[0];
    if (!backend) throw new Error('нет ни одного gguf-бэкенда: ' + backends.text.slice(0, 200));
    console.log('бэкенд: ' + backend.id + ' (' + (backend.url || backend.host || '') + ')');

    const browser = await chromium.launch({ channel: 'msedge' });
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 1000 } });
    const session = await login(BASE, process.env.VERIFY_ADMIN_USER, process.env.VERIFY_ADMIN_PASSWORD);
    if (!session) throw new Error('вход не выполнен');
    await ctx.addCookies([{ name: 'ol_session', value: session, domain: new URL(BASE).hostname, path: '/', httpOnly: true, sameSite: 'Lax' }]);

    const page = await ctx.newPage();
    const ggufRequests = [];
    page.on('request', (r) => {
        const u = r.url();
        if (u.indexOf('/api/v1/gguf/') !== -1 || u.indexOf('/api/v1/backends/') !== -1) ggufRequests.push(u.replace(BASE, ''));
    });

    await page.goto(BASE + '/?page=gguf', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#dataFreshness', { timeout: 20000 });
    await page.waitForTimeout(3500);

    // Панель деталей показывает данные ТОЛЬКО для выбранного бэкенда: сначала
    // выбираем его кликом в левом списке — ровно как оператор.
    const selected = await page.evaluate((id) => {
        const item = document.querySelector('#ggufBackendList [data-backend-id="' + id + '"]');
        if (!item) return false;
        item.click();
        return true;
    }, backend.id);
    if (!selected) throw new Error('не нашёл бэкенд ' + backend.id + ' в списке на странице');
    await page.waitForTimeout(4000);
    const beforeText = await page.evaluate(() => (document.getElementById('ggufDetailPanel') || {}).innerText || '');
    console.log('до загрузки: ' + JSON.stringify(beforeText.slice(0, 160)));
    if (beforeText.indexOf(MODEL) !== -1) {
        console.log('ВНИМАНИЕ: модель уже загружена до проверки — выгружаю и начинаю заново');
        await httpJson('POST', BASE, '/api/v1/backends/' + encodeURIComponent(backend.id) + '/models',
            { operation: 'unload', modelName: MODEL }, token);
        await page.waitForTimeout(12000);
    }

    // Грузим модель ВНЕ страницы: обычный HTTP-вызов, как из curl.
    const t0 = Date.now();
    const load = await httpJson('POST', BASE, '/api/v1/backends/' + encodeURIComponent(backend.id) + '/models',
        { operation: 'load', modelName: MODEL }, token);
    console.log('POST load → HTTP ' + load.status + ' за ' + ((Date.now() - t0) / 1000).toFixed(1) + ' с');
    if (load.status >= 400) {
        console.log('тело: ' + load.text.slice(0, 300));
        await browser.close();
        process.exit(2);
    }

    // Загруженные модели живут на вкладке «Загружено» — переключаемся на неё
    // (как оператор), а НЕ перезагружаем страницу.
    const tabOk = await page.evaluate(() => {
        const panel = document.getElementById('ggufDetailPanel');
        if (!panel) return false;
        const cand = Array.from(panel.querySelectorAll('button, [role="tab"], div, span'))
            .filter((x) => (x.textContent || '').trim().toLowerCase() === 'загружено');
        if (!cand.length) return false;
        cand[cand.length - 1].click();
        return true;
    });
    console.log('вкладка «Загружено»: ' + (tabOk ? 'открыта' : 'НЕ найдена'));
    await page.waitForTimeout(1500);

    // НЕ перезагружаем страницу. Ждём появления модели (до 60 с): и в DOM, и в
    // состоянии страницы (state.loadedModels) — чтобы отличить «данные пришли,
    // но не нарисованы» от «данные не пришли».
    let seenAt = null;
    let stateHad = false;
    const requestCountAtStart = ggufRequests.length;
    for (let i = 0; i < 30; i++) {
        await page.waitForTimeout(2000);
        const probe = await page.evaluate((model) => {
            const txt = (document.getElementById('ggufDetailPanel') || {}).innerText || '';
            const st = (window.GgufModule && window.GgufModule.state) || {};
            const loaded = Array.isArray(st.loadedModels) ? st.loadedModels : [];
            const names = loaded.map((m) => (m && (m.name || m.model)) || '');
            return { inDom: txt.indexOf(model) !== -1, inState: names.some((n) => String(n).indexOf(model) !== -1), names: names.slice(0, 5) };
        }, MODEL);
        if (probe.inState) stateHad = true;
        if (probe.inDom) { seenAt = Date.now(); break; }
        if (i === 14) console.log('  через 30 с: в состоянии=' + probe.inState + ', имена=' + JSON.stringify(probe.names));
    }
    console.log('модель в state.loadedModels: ' + stateHad);

    const elapsed = seenAt ? ((seenAt - t0) / 1000).toFixed(1) : '—';
    console.log('после загрузки (без перезагрузки страницы): ' +
        (seenAt ? 'модель появилась через ' + elapsed + ' с' : 'модель НЕ появилась за 60 с'));
    console.log('запросов страницы к /gguf|/backends за время ожидания: ' + (ggufRequests.length - requestCountAtStart));

    const afterText = await page.evaluate(() => (document.getElementById('ggufDetailPanel') || {}).innerText || '');
    console.log('фрагмент панели: ' + JSON.stringify(afterText.slice(0, 200)));

    // Выгружаем за собой, чтобы стенд остался в прежнем состоянии.
    const unload = await httpJson('POST', BASE, '/api/v1/backends/' + encodeURIComponent(backend.id) + '/models',
        { operation: 'unload', modelName: MODEL }, token);
    console.log('POST unload → HTTP ' + unload.status);

    const ok = !!seenAt;
    console.log('\nИТОГ: ' + (ok ? 'модель появилась САМА (авто-обновление работает)' : 'модель не появилась — дефект жив'));
    await browser.close();
    process.exit(ok ? 0 : 1);
})().catch((e) => { console.error('СБОЙ ПРОВЕРКИ: ' + (e && e.stack || e)); process.exit(2); });
