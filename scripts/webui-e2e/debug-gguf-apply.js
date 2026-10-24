const { chromium } = require('playwright');
(async () => {
  const b = await chromium.launch({ channel: 'msedge', headless: true });
  const page = await (await b.newContext()).newPage();
  const calls = [], dialogs = [];
  page.on('request', r => { if (r.url().includes('/api/v1/')) calls.push({ t: Date.now(), s: r.method() + ' ' + r.url().replace('http://127.0.0.1:18083','') }); });
  page.on('response', async r => {
    if (r.url().includes('/api/v1/') && r.status() >= 400) {
      let body = ''; try { body = (await r.text()).slice(0, 160); } catch (e) {}
      calls.push({ t: Date.now(), s: '!! ' + r.status() + ' ' + r.url().replace('http://127.0.0.1:18083','') + ' :: ' + body.replace(/\s+/g, ' ') });
    }
  });
  await page.goto('http://127.0.0.1:18083/index.html', { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(1800);
  await page.fill('#authName', 'ui-admin');
  await page.fill('#authPassword', 'Ui-Admin-1');
  await page.click('#authSubmit');
  await page.waitForTimeout(3500);
  await page.click('.nav-item[data-page="gguf"]');
  await page.waitForTimeout(4500);
  const pickedBackend = await page.evaluate(() => {
    const card = document.querySelector('#ggufContainer [data-backend-id]');
    if (!card) return null;
    card.click();
    return card.getAttribute('data-backend-id');
  });
  await page.waitForTimeout(3500);
  const tabs = await page.evaluate(() => Array.from(document.querySelectorAll('.gguf-detail-tab')).map(e => e.getAttribute('data-detail-tab')));
  // открыть вкладку models
  await page.evaluate(() => { const t = document.querySelector('.gguf-detail-tab[data-detail-tab="models"]'); if (t) t.click(); });
  await page.waitForTimeout(4000);
  const models = await page.evaluate(() => Array.from(document.querySelectorAll('details.gguf-mp-acc, .gguf-mp-editor')).map(e => e.getAttribute('data-model-acc') || e.getAttribute('data-model')).filter(Boolean).slice(0, 5));
  const mark = Date.now();
  // открыть карточку модели и нажать «применить и перезагрузить»
  const clicked = await page.evaluate(() => {
    const acc = document.querySelector('details.gguf-mp-acc');
    if (acc) { acc.open = true; }
    const btn = document.querySelector('[data-model-params-apply]');
    if (!btn) return { ok: false };
    const model = btn.getAttribute('data-model-params-apply');
    btn.click();
    return { ok: true, model };
  });
  await page.waitForTimeout(25000);
  const after = calls.filter(c => c.t >= mark);
  console.log('pickedBackend=' + pickedBackend + ' tabs=' + JSON.stringify(tabs) + ' models=' + JSON.stringify(models) + ' clicked=' + JSON.stringify(clicked));
  console.log('AFTER-CLICK CALLS:'); after.slice(0, 30).forEach(c => console.log('  ' + c.s));
  await b.close();
})().catch(e => console.log('FATAL ' + e.message));
