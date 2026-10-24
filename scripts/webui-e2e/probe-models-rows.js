// probe-models-rows.js — список моделей: строки, дубли, пометка загруженной.
const { chromium } = require('playwright');

(async () => {
    const url = process.argv[2] || 'http://127.0.0.1:18083';
    const browser = await chromium.launch({ channel: 'msedge', headless: true });
    const page = await browser.newPage({ viewport: { width: 1600, height: 1000 } });
    await page.goto(url + '/index.html', { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => !!document.querySelector('.nav-item[data-page="gguf"]'), null, { timeout: 30000 });
    await page.click('.nav-item[data-page="gguf"]');
    await page.waitForSelector('.gguf-backend-item', { timeout: 30000 });
    const items = await page.$$('.gguf-backend-item');
    for (const it of items) {
        const txt = (await it.innerText()).toLowerCase();
        if (txt.includes('llama') || txt.includes('cppworker')) { await it.click(); break; }
    }
    await page.waitForSelector('.gguf-detail-tab', { timeout: 30000 });
    await page.click('[data-detail-tab="models"]');
    await page.waitForSelector('details.gguf-mp-acc', { state: 'attached', timeout: 60000 });
    await page.waitForTimeout(4000);

    const rows = await page.evaluate(() => {
        const list = Array.from(document.querySelectorAll('details.gguf-mp-acc'));
        return list.map(d => ({
            acc: d.getAttribute('data-model-acc') || '',
            loaded: !!d.querySelector('.gguf-unload-btn, [data-unload]') ||
                /выгрузить/i.test(d.innerText || ''),
            loadedAs: d.getAttribute('data-loaded-as') || '',
            text: (d.innerText || '').split('\n')[0]
        }));
    });
    console.log('строк в списке: ' + rows.length);
    rows.forEach(r => console.log('  ' + r.acc + '  loaded=' + r.loaded + '  text=' + r.text +
        (r.loadedAs ? '  загружена как: ' + r.loadedAs : '')));

    const norm = s => String(s || '').replace(/\.gguf$/i, '').toLowerCase();
    const seen = {};
    const dups = [];
    rows.forEach(r => {
        const k = norm(r.acc);
        if (seen[k]) dups.push(k);
        seen[k] = true;
    });
    console.log('дублей по имени: ' + (dups.length ? dups.join(', ') : 'нет'));
    await browser.close();
})();
