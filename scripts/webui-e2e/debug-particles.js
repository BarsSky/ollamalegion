const { chromium } = require('playwright');
const http = require('http');
function fire() {
  return new Promise((resolve) => {
    const body = JSON.stringify({ model: 'gemma-4-E4B-it-Q4_K_M', stream: true, messages: [{ role: 'user', content: 'Count from 1 to 120 in words, one per line.' }], options: { num_predict: 150 } });
    const req = http.request({ host: '127.0.0.1', port: 18080, path: '/api/chat', method: 'POST', headers: { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body), 'X-API-Token': 'changeme-bundled-with-agent-token' } }, res => { res.on('data', () => {}); res.on('end', resolve); });
    req.on('error', () => resolve());
    req.end(body);
    setTimeout(resolve, 60000);
  });
}
(async () => {
  const b = await chromium.launch({ channel: 'msedge', headless: true });
  const page = await b.newPage({ viewport: { width: 1400, height: 900 } });
  const errs = []; page.on('pageerror', e => errs.push(e.message));
  await page.goto('http://127.0.0.1:18083/monitor.html', { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(3500);
  const before = await page.evaluate(() => ({ p: (window.MonitorApp.particles || []).length, s: (MonitorApp.topo.sessions || []).length }));
  fire();
  let maxP = 0, maxS = 0, samples = [];
  for (let i = 0; i < 12; i++) {
    await page.waitForTimeout(1000);
    const snap = await page.evaluate(() => ({
      p: (window.MonitorApp.particles || []).length,
      s: (MonitorApp.topo.sessions || []).length,
      a: ((MonitorApp.topo.backends || []).reduce((x, y) => x + (y.activeRequests || 0), 0))
    }));
    maxP = Math.max(maxP, snap.p); maxS = Math.max(maxS, snap.s);
    if (i < 6) samples.push(snap.p + '/' + snap.s + '/' + snap.a);
  }
  console.log('before=' + JSON.stringify(before) + ' maxParticles=' + maxP + ' maxSessions=' + maxS);
  console.log('samples(p/s/a)=' + samples.join(' '));
  console.log('errors=' + JSON.stringify(errs.slice(0, 3)));
  await b.close();
})().catch(e => { console.log('FATAL ' + e.message); });
