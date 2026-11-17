// verify-reference-page-r493.js — живая проверка страницы «Справка и шаблоны».
//
// Зачем: юнит-тесты проверяют логику модуля, а тут проверяется РЕАЛЬНАЯ страница
// в браузере против РЕАЛЬНОГО балансера (ручки /api/v1/reference/*): доехали ли
// скрипты, рисуется ли список из сида, работает ли вкладка шаблонов, есть ли
// провайдер обновления и не появилось ли второй кнопки «Обновить».
//
// Запуск: node scripts/verify-reference-page-r493.js http://127.0.0.1:18083
// Логин/пароль — из окружения (VERIFY_ADMIN_USER / VERIFY_ADMIN_PASSWORD).
'use strict';

const { chromium } = require('C:/Ollama/ollamalegion/node_modules/playwright');

const BASE = process.argv[2] || 'http://127.0.0.1:18083';

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
                resolve({ cookie: m ? m[1] : '' });
            });
        });
        req.on('error', reject);
        req.write(body);
        req.end();
    });
}

(async () => {
    const browser = await chromium.launch({ channel: 'msedge' });
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 1000 } });
    const login = await loginRequest(BASE, process.env.VERIFY_ADMIN_USER, process.env.VERIFY_ADMIN_PASSWORD);
    if (!login.cookie) throw new Error('вход не выполнен: нет cookie ol_session');
    await ctx.addCookies([{ name: 'ol_session', value: login.cookie, domain: new URL(BASE).hostname, path: '/', httpOnly: true, sameSite: 'Lax' }]);

    const page = await ctx.newPage();
    const errors = [];
    page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
    page.on('console', (m) => { if (m.type() === 'error') errors.push('console: ' + m.text()); });
    const apiCalls = [];
    const failedHttp = [];
    page.on('response', (r) => {
        if (r.url().indexOf('/api/v1/reference/') !== -1) apiCalls.push(r.status() + ' ' + r.url().replace(BASE, ''));
        if (r.status() >= 400) failedHttp.push(r.status() + ' ' + r.url().replace(BASE, ''));
    });

    let ok = true;
    const fail = (m) => { ok = false; console.log('  FAIL ' + m); };
    const pass = (m) => console.log('  OK   ' + m);

    await page.goto(BASE + '/?page=reference', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#dataFreshness', { timeout: 20000 });
    await page.waitForTimeout(3000);

    const state = await page.evaluate(() => {
        const active = document.querySelector('.page.active');
        const nav = document.querySelector('[data-page="reference"]');
        const DR = window.DataRefresh;
        const ids = (DR && DR._providers && DR._providers.reference) ? DR._providers.reference.length : 0;
        const buttons = Array.from(document.querySelectorAll('button'))
            .filter((b) => b.offsetParent !== null)
            .map((b) => (b.textContent || '').trim().toLowerCase())
            .filter((t) => t === 'обновить' || t === 'refresh').length;
        return {
            activePage: active ? active.id : '',
            hasNav: !!nav,
            providerCount: ids,
            refreshButtons: buttons,
            hasPageApi: !!(window.ReferencePage && typeof window.ReferencePage.mount === 'function'),
        };
    });

    state.activePage === 'reference-page' ? pass('страница reference открыта по ссылке ?page=reference')
        : fail('активная страница = ' + JSON.stringify(state.activePage));
    state.hasNav ? pass('пункт меню reference есть') : fail('нет пункта меню reference');
    state.hasPageApi ? pass('модуль ReferencePage загружен и экспортирован') : fail('ReferencePage не загрузился');
    state.providerCount === 1 ? pass('провайдер обновления reference зарегистрирован (1)')
        : fail('провайдеров reference: ' + state.providerCount);
    state.refreshButtons === 1 ? pass('кнопка «Обновить» на странице одна (в шапке)')
        : fail('видимых кнопок «Обновить»: ' + state.refreshButtons);

    // Содержимое: строки рекомендаций из сида (7 записей docs-import).
    const recs = await page.evaluate(() => {
        const root = document.getElementById('reference-page');
        if (!root) return { rows: 0, text: '' };
        const text = root.innerText || '';
        return {
            rows: root.querySelectorAll('[data-ref-rec], .reference-rec-row, [data-reference-id]').length,
            text: text.slice(0, 400),
        };
    });
    const hasSeedTitle = /27B|4B|gemma|DeepSeek|8B|tuning|32K/i.test(recs.text);
    recs.rows > 0 ? pass('карточек рекомендаций: ' + recs.rows) : fail('карточек рекомендаций не найдено');
    hasSeedTitle ? pass('в списке видны названия записей из сида') : fail('в списке нет ожидаемых названий (первые 400 символов: ' + JSON.stringify(recs.text) + ')');

    // Вкладка шаблонов: в разметке это обычные элементы вкладок (класс не
    // фиксирован), поэтому ищем по ТЕКСТУ и кликаем самую глубокую подходящую
    // ноду — так же, как это делает оператор.
    const tabClicked = await page.evaluate(() => {
        const root = document.getElementById('reference-page');
        if (!root) return false;
        const cand = Array.from(root.querySelectorAll('button, [role="tab"], a, div, span'))
            .filter((x) => {
                const t = (x.textContent || '').trim().toLowerCase();
                return t === 'шаблоны' || t === 'templates' || t === 'шаблоны параметров';
            });
        if (!cand.length) return false;
        cand[cand.length - 1].click();
        return true;
    });
    await page.waitForTimeout(1500);
    tabClicked ? pass('вкладка «Шаблоны» найдена и нажата') : fail('не найдена вкладка «Шаблоны»');
    const tplState = await page.evaluate(() => {
        const root = document.getElementById('reference-page');
        const text = root ? (root.innerText || '') : '';
        return { hasTplWords: /шаблон/i.test(text), len: text.length, sample: text.slice(0, 200) };
    });
    tplState.hasTplWords ? pass('после переключения на вкладке есть текст про шаблоны')
        : fail('вкладка шаблонов пуста (текст: ' + JSON.stringify(tplState.sample) + ')');

    console.log('\nзапросы к /api/v1/reference/*:');
    apiCalls.slice(0, 8).forEach((c) => console.log('  ' + c));
    const allOk = apiCalls.length > 0 && apiCalls.every((c) => c.indexOf('200') === 0 || c.indexOf('201') === 0);
    allOk ? pass('все запросы страницы к API успешны') : fail('есть неуспешные запросы к API (см. выше)');

    if (failedHttp.length) {
        console.log('\nнеуспешные HTTP-ответы:');
        failedHttp.slice(0, 10).forEach((c) => console.log('  ' + c));
    }
    // /api/v1/placement/plan отдаёт 403 сессионному администратору — это
    // ОТДЕЛЬНЫЙ, уже записанный дефект (docs/architecture/debts.ru.md п. 27),
    // к странице «Справка и шаблоны» отношения не имеет: страница его не зовёт.
    // В вердикт этой проверки он не входит, но печатается выше как факт.
    // Консольная строка про 403 не содержит URL — поэтому связываем её с
    // failedHttp: подавляем, ТОЛЬКО если все неуспешные ответы относятся к
    // placement/plan (записанный отдельно дефект, страница его не вызывает).
    const onlyUnrelatedHttp = failedHttp.length > 0 && failedHttp.every((c) => c.indexOf('placement/plan') !== -1);
    const realErrors = errors.filter((e) => {
        if (e.indexOf('/ws/') !== -1 || e.indexOf('WebSocket') !== -1) return false;
        if (onlyUnrelatedHttp && e.indexOf('403') !== -1) return false;
        return true;
    });
    realErrors.length ? fail('ошибки страницы: ' + realErrors.slice(0, 5).join(' | ')) : pass('ошибок страницы нет (кроме WS-подключений)');

    await page.screenshot({ path: require('path').join(process.env.TEMP || '.', 'reference-page-r493.png'), fullPage: false });
    console.log('\nскриншот: ' + require('path').join(process.env.TEMP || '.', 'reference-page-r493.png'));
    console.log('\nИТОГ: ' + (ok ? 'все проверки пройдены' : 'ЕСТЬ ПАДЕНИЯ'));

    await browser.close();
    process.exit(ok ? 0 : 1);
})().catch((e) => { console.error('СБОЙ ПРОВЕРКИ: ' + (e && e.stack || e)); process.exit(2); });
