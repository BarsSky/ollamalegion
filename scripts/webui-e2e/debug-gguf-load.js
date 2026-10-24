const { chromium } = require('playwright');
// R212: трассировка «Загрузить модель» и «Применить профиль» через WebUI.
(async () => {
  const b = await chromium.launch({ channel: 'msedge', headless: true });
  const page = await (await b.newContext()).newPage();
  const calls = [], errors = [];
  page.on('pageerror', e => errors.push('pageerror: ' + e.message));
  page.on('console', m => { if (m.type() === 'error') errors.push('console: ' + m.text()); });
  page.on('request', r => { if (r.url().includes('/api/')) calls.push(r.method() + ' ' + r.url().replace('http://127.0.0.1:18083','')); });
  page.on('response', async r => {
    if (r.url().includes('/api/') && r.status() >= 400) {
      let body = ''; try { body = (await r.text()).slice(0, 200); } catch (e) {}
      calls.push('  !! ' + r.status() + ' ' + r.url().replace('http://127.0.0.1:18083','') + ' :: ' + body.replace(/\s+/g,' '));
    }
  });
  await page.goto('http://127.0.0.1:18083/index.html', { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(2000);
  await page.fill('#authName', 'ui-admin');
  await page.fill('#authPassword', 'Ui-Admin-1');
  await page.click('#authSubmit');
  await page.waitForTimeout(4000);
  await page.click('.nav-item[data-page="gguf"]');
  await page.waitForTimeout(5000);
  // выбрали бэкенд (в списке он один)
  const picked = await page.evaluate(() => {
    const card = document.querySelector('#ggufContainer [data-backend-id]');
    if (card) { card.click(); return card.getAttribute('data-backend-id'); }
    return null;
  });
  await page.waitForTimeout(4000);
  // вкладка «Модели» внутри GGUF и кнопка загрузки (если есть)
  const buttons = await page.evaluate(() => {
    const out = [];
    document.querySelectorAll('button, [role=button]').forEach(el => {
      const t = (el.innerText || '').trim();
      if (t && /загруз|load|примен/i.test(t)) out.push({ text: t.slice(0, 40), id: el.id || '', cls: (el.className || '').slice(0, 60) });
    });
    return out.slice(0, 12);
  });
  console.log(JSON.stringify({ picked, buttons, errors: errors.slice(0, 5) }, null, 1));
  const apiCalls = calls.filter(c => !c.includes('/auth/') && !c.includes('/api/v1/backends ') && !c.includes('/api/v1/metrics'));
  console.log('API CALLS:'); apiCalls.slice(0, 40).forEach(c => console.log('  ' + c));
  await b.close();
})().catch(e => console.log('FATAL ' + e.message));
