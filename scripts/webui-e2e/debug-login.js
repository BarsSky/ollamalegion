const { chromium } = require('playwright');
(async () => {
  const b = await chromium.launch({ channel: 'msedge', headless: true });
  const page = await (await b.newContext()).newPage();
  await page.goto('http://127.0.0.1:18083/index.html', { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(2000);
  const before = await page.evaluate(() => ({
    overlay: (function(){var el=document.getElementById('authOverlay');return el?el.style.display:null;})(),
    usersConfigured: window.Auth && window.Auth.state().usersConfigured
  }));
  await page.fill('#authName', 'ui-check');
  await page.fill('#authPassword', 'Ui-Check-1');
  await page.click('#authSubmit');
  await page.waitForTimeout(5000);
  const after = await page.evaluate(() => ({
    role: window.Auth ? window.Auth.role() : null,
    overlay: (function(){var el=document.getElementById('authOverlay');return el?el.style.display:null;})(),
    nav: !!document.querySelector('.nav-item[data-page="gguf"]')
  }));
  console.log(JSON.stringify({ before, after }, null, 1));
  await b.close();
})().catch(e => console.log('FATAL ' + e.message));
