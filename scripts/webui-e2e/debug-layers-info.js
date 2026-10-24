const { chromium } = require('playwright');
(async () => {
  const b = await chromium.launch({ channel: 'msedge', headless: true });
  const ctx = await b.newContext();
  const page = await ctx.newPage();
  const errs = []; page.on('pageerror', e => errs.push(e.message));
  page.on('console', m => { if (m.type() === 'error') errs.push('console: ' + m.text()); });
  await page.goto('http://127.0.0.1:18083/index.html', { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(2500);
  // вход под админом невозможен без пароля оператора — проверяем как гость:
  // экран входа не должен блокировать анализ модуля, поэтому смотрим только ошибки
  const info = await page.evaluate(() => ({
    moduleLoaded: typeof window.LayoutLayersInfo === 'object',
    container: !!document.getElementById('layoutLayersInfo'),
    display: (function(){ var el=document.getElementById('layoutLayersInfo'); return el ? el.style.display : null; })(),
    hasText: (function(){ var el=document.getElementById('layoutLayersInfo'); return el ? el.textContent : null; })()
  }));
  console.log(JSON.stringify({ info, errors: errs.slice(0, 4) }, null, 1));
  await b.close();
})().catch(e => console.log('FATAL ' + e.message));
