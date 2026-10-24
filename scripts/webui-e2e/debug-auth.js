const { chromium } = require('playwright');
(async () => {
  const b = await chromium.launch({ channel: 'msedge', headless: true });
  const out = {};
  // контекст 1: анонимный — должен увидеть экран входа
  const anon = await b.newContext();
  const p1 = await anon.newPage();
  const errs = [];
  p1.on('pageerror', e => errs.push(e.message));
  await p1.goto('http://127.0.0.1:18083/index.html', { waitUntil: 'domcontentloaded' });
  await p1.waitForTimeout(2500);
  out.anonOverlay = await p1.evaluate(() => {
    const el = document.getElementById('authOverlay');
    return el ? (el.style.display !== 'none') : false;
  });
  out.anonState = await p1.evaluate(() => (window.Auth && window.Auth.state) ? window.Auth.state() : null);
  // вход администратором
  await p1.fill('#authName', 'admin');
  await p1.fill('#authPassword', 'Legion-t4XyrmSPx9G6MoDd');
  await p1.click('#authSubmit');
  await p1.waitForTimeout(3500);
  out.afterLogin = await p1.evaluate(() => ({
    overlay: (function () { const el = document.getElementById('authOverlay'); return el ? el.style.display : 'none'; })(),
    state: (window.Auth && window.Auth.state) ? window.Auth.state() : null,
    chip: !!document.getElementById('authUserChip')
  }));
  out.errors = errs.slice(0, 5);

  // контекст 2: viewer — вход и проверка, что роль видна
  const v = await b.newContext();
  const p2 = await v.newPage();
  await p2.goto('http://127.0.0.1:18083/index.html', { waitUntil: 'domcontentloaded' });
  await p2.waitForTimeout(2500);
  await p2.fill('#authName', 'guest');
  await p2.fill('#authPassword', 'viewer-pass-1');
  await p2.click('#authSubmit');
  await p2.waitForTimeout(3500);
  out.viewer = await p2.evaluate(() => ({
    state: (window.Auth && window.Auth.state) ? window.Auth.state() : null,
    isAdmin: window.Auth && window.Auth.isAdmin ? window.Auth.isAdmin() : null
  }));
  console.log(JSON.stringify(out, null, 1));
  await b.close();
})().catch(e => { console.log('FATAL ' + e.message); });
