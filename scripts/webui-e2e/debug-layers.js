const { chromium } = require('playwright');
(async () => {
  const b = await chromium.launch({ channel: 'msedge', headless: true });
  const ctx = await b.newContext();
  const page = await ctx.newPage();
  const errs = []; page.on('pageerror', e => errs.push(e.message));
  await page.goto('http://127.0.0.1:18083/monitor.html', { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(2500);
  await page.fill('#authName', 'admin');
  await page.fill('#authPassword', 'Legion-t4XyrmSPx9G6MoDd');
  await page.click('#authSubmit');
  await page.waitForTimeout(6000);
  const info = await page.evaluate(() => {
    const el = document.getElementById('layoutLayers');
    const badge = document.getElementById('layoutStateBadge');
    return {
      role: window.Auth && window.Auth.role ? window.Auth.role() : null,
      layers: el ? el.textContent : null,
      badge: badge ? badge.textContent : null
    };
  });
  console.log(JSON.stringify({ info, errors: errs.slice(0, 3) }, null, 1));
  await b.close();
})().catch(e => console.log('FATAL ' + e.message));
