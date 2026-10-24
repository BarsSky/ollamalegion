const { chromium } = require('playwright');
(async () => {
  const b = await chromium.launch({ channel: 'msedge', headless: true });
  const out = {};
  async function loginAs(user, pass) {
    const ctx = await b.newContext();
    const page = await ctx.newPage();
    const errs = []; page.on('pageerror', e => errs.push(e.message));
    await page.goto('http://127.0.0.1:18083/monitor.html', { waitUntil: 'domcontentloaded' });
    await page.waitForTimeout(2500);
    await page.fill('#authName', user);
    await page.fill('#authPassword', pass);
    await page.click('#authSubmit');
    await page.waitForTimeout(3500);
    const r = await page.evaluate(() => {
      const apply = document.getElementById('layoutApplyBtn');
      return {
        role: window.Auth && window.Auth.role ? window.Auth.role() : null,
        bodyClass: document.body.className,
        layoutApply: apply ? { display: apply.style.display, disabled: apply.hasAttribute('disabled') } : null
      };
    });
    r.errors = errs.slice(0, 3);
    await ctx.close();
    return r;
  }
  out.admin = await loginAs('admin', 'Legion-t4XyrmSPx9G6MoDd');
  out.viewer = await loginAs('guest', 'viewer-pass-1');
  console.log(JSON.stringify(out, null, 1));
  await b.close();
})().catch(e => { console.log('FATAL ' + e.message); });
