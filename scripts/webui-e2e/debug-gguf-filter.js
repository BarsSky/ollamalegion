const { chromium } = require('playwright');
(async () => {
  const b = await chromium.launch({ channel: 'msedge', headless: true });
  const ctx = await b.newContext();
  const page = await ctx.newPage();
  const errs = []; page.on('pageerror', e => errs.push(e.message));
  await page.goto('http://127.0.0.1:18083/index.html', { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(2500);
  await page.fill('#authName', 'ui-check');
  await page.fill('#authPassword', 'Ui-Check-1');
  await page.click('#authSubmit');
  await page.waitForTimeout(3500);
  // переходим на GGUF Models
  await page.click('.nav-item[data-page="gguf"]');
  await page.waitForSelector('#ggufBackendSelect', { timeout: 20000 });
  const opts = await page.$$eval('#ggufBackendSelect option', els => els.map(e => e.value));
  const cards = await page.$$eval('#ggufBackendCards [data-backend-id]', els => els.map(e => e.getAttribute('data-backend-id'))).catch(() => []);
  console.log(JSON.stringify({ options: opts, cards: cards, hasImageworker: opts.concat(cards).some(x => String(x).includes('imageworker')), errors: errs.slice(0,3) }, null, 1));
  await b.close();
})().catch(e => console.log('FATAL ' + e.message));
