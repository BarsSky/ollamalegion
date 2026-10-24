const { chromium } = require('playwright');
(async () => {
  const b = await chromium.launch({ channel: 'msedge', headless: true });
  const page = await (await b.newContext()).newPage();
  const errs = []; page.on('pageerror', e => errs.push(e.message));
  await page.goto('http://127.0.0.1:18083/index.html', { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(2000);
  await page.fill('#authName', 'ui-check');
  await page.fill('#authPassword', 'Ui-Check-1');
  await page.click('#authSubmit');
  await page.waitForTimeout(4000);
  await page.click('.nav-item[data-page="gguf"]');
  await page.waitForTimeout(5000);
  const info = await page.evaluate(() => {
    const sel = document.getElementById('ggufBackendSelect');
    const opts = sel ? Array.from(sel.options).map(o => o.value) : null;
    const container = document.getElementById('ggufContainer');
    const cards = Array.from(document.querySelectorAll('#ggufContainer [data-backend-id]')).map(e => e.getAttribute('data-backend-id'));
    return {
      hasSelect: !!sel, options: opts, cards: cards,
      text: container ? container.innerText.slice(0, 200).replace(/\s+/g, ' ') : null
    };
  });
  info.errors = errs.slice(0, 3);
  info.hasImageworker = [].concat(info.options || [], info.cards || []).some(x => String(x).includes('imageworker'));
  console.log(JSON.stringify(info, null, 1));
  await b.close();
})().catch(e => console.log('FATAL ' + e.message));
