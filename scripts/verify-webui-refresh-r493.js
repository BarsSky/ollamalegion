// verify-webui-r493.js — живая проверка правок R493 на работающем WebUI.
//
// Что проверяем (по жалобам оператора):
//   Д1: на каждой странице РОВНО ОДНА кнопка «Обновить» — в шапке.
//   Д3: авто-обновление само обновляет данные активной страницы (провайдеры
//       вызываются из тика, а не только из кнопки).
//   Д2: у страницы GGUF провайдер зарегистрирован и функции рендерера реально
//       существуют (иначе «обновить» нечего).
//
// Запуск: node <этот файл> http://127.0.0.1:18085
'use strict';

const { chromium } = require('C:/Ollama/ollamalegion/node_modules/playwright');

const BASE = process.argv[2] || 'http://127.0.0.1:18085';

function log(s) { console.log(s); }

(async () => {
    // Chromium из node_modules в этом окружении не стартует страницу (newPage
    // висит >15 с — проверено и подтверждено диагностикой), поэтому берём
    // установленный Edge через канал msedge.
    const browser = await chromium.launch({ channel: 'msedge' });
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 1000 } });
    const page = await ctx.newPage();

    // Вход. WebUI с R222 ходит СЕССИЕЙ (cookie ol_session), токен в страницу не
    // вшивается (entrypoint.sh: API_TOKEN: ''): без входа страница получает 401
    // и показывает экран входа. Поэтому входим тем же HTTP-путём, что оператор,
    // и кладём cookie в контекст браузера ДО первой навигации.
    // Логин/пароль приходят из окружения и в репозиторий не попадают:
    // VERIFY_ADMIN_USER / VERIFY_ADMIN_PASSWORD.
    const adminUser = process.env.VERIFY_ADMIN_USER || '';
    const adminPass = process.env.VERIFY_ADMIN_PASSWORD || '';
    if (adminUser && adminPass) {
        const login = await loginRequest(BASE, adminUser, adminPass);
        if (!login.cookie) throw new Error('не удалось войти: сервер не вернул cookie ol_session');
        // Playwright: либо url, либо domain+path — вместе нельзя.
        const host = new URL(BASE).hostname;
        await ctx.addCookies([{
            name: 'ol_session', value: login.cookie, domain: host, path: '/',
            httpOnly: true, sameSite: 'Lax',
        }]);
        log('вход выполнен: ' + login.user + ' (роль ' + login.role + ')');
    } else {
        log('ВНИМАНИЕ: VERIFY_ADMIN_USER/VERIFY_ADMIN_PASSWORD не заданы — страница останется без сессии (401)');
    }

    const errors = [];
    page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
    page.on('console', (m) => { if (m.type() === 'error') errors.push('console: ' + m.text()); });

    // Считаем запросы к источникам страниц — по ним видно, работает ли авто-тик.
    const requests = [];
    page.on('request', (r) => {
        const u = r.url();
        if (u.indexOf('/api/') !== -1) requests.push({ t: Date.now(), url: u.replace(BASE, '') });
    });

    await page.goto(BASE + '/?page=dashboard', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#dataFreshness', { timeout: 20000 });
    await page.waitForTimeout(2500);

    log('=== Д1: кнопки «Обновить» по страницам');
    const pages = ['dashboard', 'backends', 'models', 'gguf', 'image', 'diagnostics', 'settings', 'queue', 'sessions', 'logs'];
    let d1ok = true;
    for (const p of pages) {
        // Переключаемся тем же путём, что оператор: клик по пункту меню.
        // (window.App.switchPage наружу не экспортирован — есть только
        // App.context.switchPage; проверять внутренний вызов менее честно.)
        var clicked = false;
        try {
            await page.click('[data-page="' + p + '"]', { timeout: 3000 });
            clicked = true;
        } catch (e) { clicked = false; }
        if (!clicked) {
            await page.evaluate((pg) => window.App.context.switchPage(pg), p);
        }
        await page.waitForTimeout(400);
        const info = await page.evaluate(() => {
            const all = Array.from(document.querySelectorAll('button, a.btn'));
            const visible = all.filter((b) => {
                if (b.offsetParent === null) return false;
                const t = (b.textContent || '').trim().toLowerCase();
                return t === 'обновить' || t === 'refresh';
            });
            return {
                count: visible.length,
                ids: visible.map((b) => b.id || '(без id)'),
                headerId: (document.getElementById('refreshBtn') || {}).id || null,
            };
        });
        const active = await page.evaluate(() => {
            const a = document.querySelector('.page.active');
            return a ? a.id : '(нет активной страницы)';
        });
        const ok = info.count === 1 && info.headerId === 'refreshBtn' && info.ids[0] === 'refreshBtn';
        log('        активная страница: ' + active);
        if (!ok) d1ok = false;
        log(`  ${ok ? 'OK  ' : 'FAIL'} ${p}: видимых кнопок «Обновить» = ${info.count} [${info.ids.join(', ')}]`);
    }

    log('\n=== Д3: авто-обновление само тянет источники активной страницы');
    await switchTo(page, 'diagnostics');
    await page.waitForTimeout(1200);
    const beforeCount = requests.length;
    const at1 = await page.evaluate(() => (document.getElementById('diagLoadedAt') || {}).textContent || '');
    const summaryBefore = requests.filter((r) => r.url.indexOf('diag/summary') !== -1).length;
    await page.waitForTimeout(12000);
    const at2 = await page.evaluate(() => (document.getElementById('diagLoadedAt') || {}).textContent || '');
    const summaryAfter = requests.filter((r) => r.url.indexOf('diag/summary') !== -1).length;
    const requestsDuring = requests.length - beforeCount;
    log(`  подпись «загружено» до:  ${JSON.stringify(at1)}`);
    log(`  подпись «загружено» после 12 с: ${JSON.stringify(at2)}`);
    log(`  запросов за 12 с: ${requestsDuring}, из них /diag/summary: ${summaryAfter - summaryBefore}`);
    // Не больше одного запроса на тик: дубликат означал бы, что провайдер
    // зарегистрирован несколько раз (живой дефект: mount() регистрировал
    // провайдера на каждый показ страницы → 6 запросов за 12 с).
    const ticks = Math.max(1, Math.round(12 / 5));
    const d3ok = summaryAfter - summaryBefore >= 1 && summaryAfter - summaryBefore <= ticks + 2 && at2 !== at1;
    log(`  ${d3ok ? 'OK  ' : 'FAIL'} авто-обновление вызывает провайдера страницы (было ${summaryBefore}, стало ${summaryAfter})`);

    log('\n=== Д2: провайдер GGUF и функции рендерера');
    await switchTo(page, 'gguf');
    await page.waitForSelector('#dataFreshness', { timeout: 20000 });
    await page.waitForTimeout(2500);
    const gguf = await page.evaluate(() => ({
        providers: Object.keys((window.DataRefresh && window.DataRefresh._providers) || {}),
        hasModule: !!window.GgufModule,
        refreshBackends: typeof (window.GgufModule || {}).refreshBackends,
        refreshDetail: typeof (window.GgufModule || {}).refreshDetail,
        refreshActiveDownloads: typeof (window.GgufModule || {}).refreshActiveDownloads,
        loadedRows: document.querySelectorAll('#ggufDetailPanel [data-loaded-model], #ggufBackendList .gguf-loaded-chip').length,
    }));
    log('  ' + JSON.stringify(gguf));
    const d2ok = gguf.providers.indexOf('gguf') !== -1 && gguf.refreshBackends === 'function' && gguf.refreshDetail === 'function';

    log('\n=== кнопка в шапке действительно дёргает провайдеров');
    await switchTo(page, 'diagnostics');
    await page.waitForTimeout(600);
    const before = requests.filter((r) => r.url.indexOf('diag/summary') !== -1).length;
    await page.click('#refreshBtn');
    await page.waitForTimeout(2500);
    const after = requests.filter((r) => r.url.indexOf('diag/summary') !== -1).length;
    const btnOk = after > before;
    log(`  ${btnOk ? 'OK  ' : 'FAIL'} после клика #refreshBtn запросов /diag/summary: ${before} → ${after}`);

    log('\n=== ошибки страницы');
    log(errors.length ? errors.slice(0, 10).map((e) => '  ' + e).join('\n') : '  нет');

    // --- регрессии, найденные живым замером диагностики ---------------------
    log('\n=== скрытая страница не перерисовывается тиком (было 88 мутаций за 20 с)');
    await switchTo(page, 'backends');
    await page.waitForTimeout(800);
    const mutsDuring = await page.evaluate(async () => {
        const dash = document.getElementById('dashboard-page');
        let dashMuts = 0;
        const obs = new MutationObserver((list) => { dashMuts += list.length; });
        obs.observe(dash, { childList: true, subtree: true, attributes: true, characterData: true });
        const backends = document.getElementById('backends-page');
        let activeMuts = 0;
        const obs2 = new MutationObserver((list) => { activeMuts += list.length; });
        obs2.observe(backends, { childList: true, subtree: true, attributes: true, characterData: true });
        await new Promise((r) => setTimeout(r, 12000));
        obs.disconnect();
        obs2.disconnect();
        return { dashMuts, activeMuts, ctxPage: (window.App.context || {}).currentPage };
    });
    log(`  ctx.currentPage = ${JSON.stringify(mutsDuring.ctxPage)} (должно быть "backends", а не "dashboard")`);
    log(`  мутаций скрытого dashboard: ${mutsDuring.dashMuts}, активной страницы backends: ${mutsDuring.activeMuts}`);
    // Порог не 0: остаточные 8 мутаций за 12 с приходят от WebSocket-событий,
    // которые пишут метрики в скрытые узлы дашборда (Utils.setText по id) — это
    // отдельный класс, в этой правке не закрыт (было 88 при живом замере).
    const liveOk = mutsDuring.ctxPage === 'backends' && mutsDuring.dashMuts <= 12 && mutsDuring.activeMuts > 0;
    log(`  ${liveOk ? 'OK  ' : 'FAIL'} тик обновляет активную страницу и не трогает скрытую`);

    log('\n=== монитор не опрашивает API, пока его страница не открыта');
    const monBefore = requests.filter((r) => r.url.indexOf('/monitor') !== -1 || r.url.indexOf('fetchAll') !== -1).length;
    const beforeAll = requests.length;
    await page.waitForTimeout(8000);
    const newReqs = requests.slice(beforeAll);
    const monReqs = newReqs.filter((r) => r.url.indexOf('/api/') !== -1 && (r.url.indexOf('monitor') !== -1)).length;
    log(`  всего запросов за 8 с на странице backends: ${newReqs.length}`);
    const monOk = monReqs === 0;
    log(`  ${monOk ? 'OK  ' : 'FAIL'} запросов, привязанных к монитору, без открытой страницы монитора: ${monReqs}`);

    log('\n=== GGUF: один запрос списка бэкендов за тик, без пересборки страницы');
    await switchTo(page, 'gguf');
    await page.waitForTimeout(2500);
    const idx = requests.length;
    await page.waitForTimeout(11000);
    const tickReqs = requests.slice(idx).filter((r) => r.url.indexOf('/api/v1/gguf/backends') !== -1);
    const ggufTicks = Math.round(11000 / (window0Interval()), 1);
    log(`  запросов /api/v1/gguf/backends за 11 с: ${tickReqs.length}`);
    const noDupOk = tickReqs.length <= 3; // ~2 тика, по одному запросу на тик
    log(`  ${noDupOk ? 'OK  ' : 'FAIL'} запросов списка GGUF: ${tickReqs.length} (ожидалось ≤3 при тике 5 с)`);

    log('\nИТОГ: Д1=' + (d1ok ? 'OK' : 'FAIL') + ', Д3=' + (d3ok ? 'OK' : 'FAIL') + ', Д2=' + (d2ok ? 'OK' : 'FAIL') +
        ', кнопка=' + (btnOk ? 'OK' : 'FAIL') + ', ctx=' + (liveOk ? 'OK' : 'FAIL') +
        ', монитор=' + (monOk ? 'OK' : 'FAIL') + ', gguf=' + (noDupOk ? 'OK' : 'FAIL'));
    await browser.close();
    process.exit(d1ok && d3ok && d2ok && btnOk && liveOk && monOk && noDupOk ? 0 : 1);
})().catch((e) => { console.error('СБОЙ ПРОВЕРКИ: ' + (e && e.stack || e)); process.exit(2); });

async function switchTo(page, name) {
    try {
        await page.click('[data-page="' + name + '"]', { timeout: 3000 });
    } catch (e) {
        await page.evaluate((pg) => window.App.context.switchPage(pg), name);
    }
}

/**
 * loginRequest — POST /api/v1/auth/login и разбор Set-Cookie.
 *
 * Зачем свой HTTP-вызов, а не экран входа: проверка не должна зависеть от
 * вёрстки формы, и cookie обязана стоять ДО первой навигации.
 */
function loginRequest(base, user, pass) {
    return new Promise((resolve, reject) => {
        const body = JSON.stringify({ name: user, password: pass });
        const url = new URL(base + '/api/v1/auth/login');
        const req = require('http').request({
            hostname: url.hostname,
            port: url.port || 80,
            path: url.pathname,
            method: 'POST',
            headers: { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) },
        }, (res) => {
            let data = '';
            res.on('data', (c) => { data += c; });
            res.on('end', () => {
                const raw = res.headers['set-cookie'] || [];
                const line = raw.find((c) => c.indexOf('ol_session=') === 0) || '';
                const m = line.match(/ol_session=([^;]+)/);
                let payload = {};
                try { payload = JSON.parse(data); } catch (e) { /* не JSON — оставим пусто */ }
                resolve({
                    cookie: m ? m[1] : '',
                    user: (payload.user && payload.user.name) || '?',
                    role: (payload.user && payload.user.role) || '?',
                });
            });
        });
        req.on('error', reject);
        req.write(body);
        req.end();
    });
}

function window0Interval() { return 5000; }
