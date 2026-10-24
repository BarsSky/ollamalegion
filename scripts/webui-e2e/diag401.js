const { chromium } = require('playwright');
(async () => {
  const b = await chromium.launch({ channel: 'msedge', headless: true });
  const ctx = await b.newContext();
  const page = await ctx.newPage();
  const bad = [];
  page.on('response', r => { if (r.status() === 401 || r.status() === 403) bad.push(r.status() + ' ' + r.url()); });
  await page.goto('http://127.0.0.1:18083/index.html', { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(4000);
  console.log(JSON.stringify(bad.slice(0, 8), null, 1));
  await b.close();
})().catch(e => console.log('FATAL ' + e.message));
