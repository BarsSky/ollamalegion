// probe-merge-link.js — R166: в панели «Объединение бэкендов» есть поле канала и
// кнопка замера; замер реально возвращает числа и обновляет подсказку.
const { chromium } = require('playwright');

(async () => {
    const url = process.argv[2] || 'http://127.0.0.1:18083';
    const browser = await chromium.launch({ channel: 'msedge', headless: true });
    const page = await browser.newPage({ viewport: { width: 1600, height: 1000 } });
    const errors = [];
    page.on('pageerror', e => errors.push('pageerror: ' + e.message));
    page.on('console', m => { if (m.type() === 'error') errors.push('console: ' + m.text()); });

    await page.goto(url + '/index.html', { waitUntil: 'domcontentloaded' });
    await page.waitForTimeout(4000);

    const present = await page.evaluate(() => ({
        panel: !!document.getElementById('backendMergePanel'),
        field: !!document.getElementById('bmLinkMbps'),
        button: !!document.getElementById('bmMeasure'),
        hint: (document.querySelector('#backendMergePanel .bm-hint') || {}).innerText || ''
    }));
    console.log('панель/поле/кнопка: ' + JSON.stringify(present));

    if (!present.button) { console.log('кнопки нет — дальше не проверяю'); await browser.close(); return; }

    // Клик через DOM: кнопка живёт в окне, которое в пробе не открыто, и обычный
    // click() ждёт видимости. Нам нужен сам обработчик, а не геометрия.
    await page.evaluate(() => document.getElementById('bmMeasure').click());
    await page.waitForTimeout(5000);
    const after = await page.evaluate(() => {
        const hints = Array.from(document.querySelectorAll('#backendMergePanel .bm-hint'))
            .map(e => (e.innerText || '').trim()).filter(Boolean);
        return { hints: hints.slice(0, 3), fieldValue: (document.getElementById('bmLinkMbps') || {}).value };
    });
    console.log('подсказки после замера: ' + JSON.stringify(after.hints));
    console.log('поле (пусто = авто): "' + after.fieldValue + '"');
    console.log('ошибок консоли: ' + errors.length);
    errors.slice(0, 3).forEach(e => console.log('  ' + e));
    await browser.close();
})();
