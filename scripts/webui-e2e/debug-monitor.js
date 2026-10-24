const { chromium } = require('playwright');
(async () => {
  const b = await chromium.launch({ channel: 'msedge', headless: true });
  const page = await b.newPage({ viewport: { width: 1400, height: 900 } });
  const logs = [];
  page.on('pageerror', e => logs.push('pageerror: ' + e.message));
  page.on('console', m => { if (m.type() !== 'log') logs.push(m.type() + ': ' + m.text()); });
  await page.goto('http://127.0.0.1:18083/monitor.html', { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(5000);
  const info = await page.evaluate(() => ({
    hasDraw: typeof window.drawTopo === 'function',
    hasSpawn: typeof window.MonitorApp !== 'undefined',
    sessions: (window.MonitorApp.topo.sessions || []).length,
    particles: (window.MonitorApp.particles || []).length,
    drawLen: (window.drawTopo || '').toString().length,
    hasR200: (window.drawTopo || '').toString().indexOf('R200') >= 0
  }));
  console.log('INFO ' + JSON.stringify(info));
  console.log('LOGS ' + JSON.stringify(logs.slice(0, 6)));
  await b.close();
})();
