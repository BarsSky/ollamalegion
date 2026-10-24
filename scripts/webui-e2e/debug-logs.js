const { chromium } = require('playwright');
// R208: длинное сообщение об ошибке должно переноситься, а не обрезаться.
(async () => {
  const b = await chromium.launch({ channel: 'msedge', headless: true });
  const page = await b.newPage({ viewport: { width: 1200, height: 900 } });
  await page.goto('http://127.0.0.1:18083/index.html', { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(1500);
  const res = await page.evaluate(() => {
    const host = document.createElement('div');
    host.className = 'logs-container';
    host.id = 'logsContainer';
    host.style.cssText = 'width:900px;max-height:400px;overflow-y:auto;padding:12px';
    const long = 'panic: runtime error: invalid memory address or nil pointer dereference [signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x8b3f21] goroutine 42 [running]: main.(*Proxy).serveHTTP(0xc0001a2000, {0x1c2a3b0, 0xc0000b4000}, 0xc0000e8000)';
    host.innerHTML = '<div class="log-entry"><span class="log-time">12:00:00</span><span class="log-level error">error</span><span class="log-message">' + long + '</span></div>';
    document.body.appendChild(host);
    const entry = host.querySelector('.log-entry');
    const msg = host.querySelector('.log-message');
    const e = entry.getBoundingClientRect();
    const m = msg.getBoundingClientRect();
    return {
      entryOverflows: entry.scrollWidth > entry.clientWidth + 1,
      msgLines: Math.round(m.height / 18),
      msgVisibleWidth: Math.round(m.width),
      titleLen: (entry.getAttribute('title') || '').length,
      clippedByContainer: m.right > host.getBoundingClientRect().right + 1
    };
  });
  console.log(JSON.stringify(res, null, 1));
  await b.close();
})().catch(e => console.log('FATAL ' + e.message));
