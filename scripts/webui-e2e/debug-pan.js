// debug-pan.js — временная диагностика панорамирования монитора (R199).
const { chromium } = require('playwright');
(async () => {
  const browser = await chromium.launch({ channel: 'msedge', headless: true });
  const page = await browser.newPage({ viewport: { width: 1400, height: 900 } });
  const errs = [];
  page.on('pageerror', e => errs.push('pageerror: ' + e.message));
  page.on('console', m => { if (m.type() === 'error') errs.push('console: ' + m.text()); });
  await page.goto('http://127.0.0.1:18083/monitor.html', { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(4000);

  const info = await page.evaluate(() => {
    const cv = document.getElementById('vizCanvas');
    const vpBox = document.querySelector('.topo-viewport');
    const r = cv ? cv.getBoundingClientRect() : null;
    const px = r ? Math.round(r.left + r.width / 2) : 0;
    const py = r ? Math.round(r.top + r.height / 2) : 0;
    const el = document.elementFromPoint(px, py);
    const chain = [];
    let cur = el;
    while (cur && chain.length < 6) { chain.push(cur.tagName + (cur.id ? '#' + cur.id : '') + (cur.className && typeof cur.className === 'string' ? '.' + cur.className.split(' ')[0] : '')); cur = cur.parentElement; }
    const host = cv ? cv.closest('.topo-viewport') : null;
    return {
      canvas: !!cv, canvasRect: r ? { w: r.width, h: r.height, left: r.left, top: r.top } : null,
      host: host ? host.className : null,
      point: [px, py], elementAtPoint: el ? el.tagName + (el.id ? '#' + el.id : '') : null,
      chain,
      hostCursor: host ? host.style.cursor : null,
      viewportBefore: window.MonitorApp ? { x: MonitorApp.viewport.offsetX, y: MonitorApp.viewport.offsetY, s: MonitorApp.viewport.scale } : null,
      hasToWorld: !!(window.MonitorApp && MonitorApp.toWorld)
    };
  });
  console.log('INFO ' + JSON.stringify(info));

  const [px, py] = info.point;
  await page.mouse.move(px, py);
  await page.mouse.down();
  await page.waitForTimeout(120);
  const mid = await page.evaluate(() => ({
    userSelect: document.body.style.userSelect,
    hostCursor: (function(){ const cv=document.getElementById('vizCanvas'); const h=cv&&cv.closest('.topo-viewport'); return h?h.style.cursor:null; })(),
    vp: window.MonitorApp ? { x: MonitorApp.viewport.offsetX, y: MonitorApp.viewport.offsetY } : null
  }));
  console.log('AFTER_DOWN ' + JSON.stringify(mid));
  for (let i = 1; i <= 5; i++) { await page.mouse.move(px + i * 30, py + i * 15); await page.waitForTimeout(60); }
  const afterMove = await page.evaluate(() => (window.MonitorApp ? { x: MonitorApp.viewport.offsetX, y: MonitorApp.viewport.offsetY } : null));
  console.log('AFTER_MOVE ' + JSON.stringify(afterMove));
  await page.mouse.up();
  const afterUp = await page.evaluate(() => ({
    vp: window.MonitorApp ? { x: MonitorApp.viewport.offsetX, y: MonitorApp.viewport.offsetY } : null,
    userSelect: document.body.style.userSelect
  }));
  console.log('AFTER_UP ' + JSON.stringify(afterUp));
  console.log('ERRORS ' + JSON.stringify(errs.slice(0, 8)));
  await browser.close();
})().catch(e => { console.log('FATAL ' + e.message); process.exit(1); });
