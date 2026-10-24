// debug-gguf-apply-kv-r213.js — R213 (2026-10-21).
//
// Проверяет ГЛАВНУЮ жалобу оператора: «параметры, выставленные в WebUI, не
// применяются по кнопкам».
//
// Сценарий на загруженной модели:
//   1) открыть карточку загруженной модели на странице GGUF;
//   2) выставить kvCacheType=q8_0 в форме параметров и нажать «применить»;
//   3) дождаться PUT профиля (200) и перезагрузки модели;
//   4) прочитать факт из воркера: kv_cache_type обязан стать q8_0.
//
// Запуск: node scripts/webui-e2e/debug-gguf-apply-kv-r213.js [q8_0|q4_0]
const { chromium } = require('playwright');

const WANT = (process.argv[2] || 'q8_0').toLowerCase();
const MODEL = 'gemma-4-E4B-it-Q4_K_M.gguf';
const sleep = ms => new Promise(r => setTimeout(r, ms));

(async () => {
  const b = await chromium.launch({ channel: 'msedge', headless: true });
  const page = await (await b.newContext()).newPage();
  const errs = [];
  const calls = [];
  page.on('pageerror', e => errs.push(e.message));
  page.on('response', async r => {
    const u = r.url();
    if (!/api\/v1\/cppworker\/model-profiles|api\/models\/reload|api\/models\/load/.test(u)) return;
    let body = '';
    try { body = (await r.text()).replace(/\s+/g, ' ').slice(0, 200); } catch (e) {}
    calls.push(r.status() + ' ' + r.request().method() + ' ' + u.replace('http://127.0.0.1:18083', '') + ' :: ' + body);
  });

  await page.goto('http://127.0.0.1:18083/index.html', { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(2000);
  await page.fill('#authName', 'ui-check');
  await page.fill('#authPassword', 'Ui-Check-1');
  await page.click('#authSubmit');
  await page.waitForTimeout(3500);

  await page.click('.nav-item[data-page="gguf"]');
  await page.waitForTimeout(4500);
  await page.evaluate(() => {
    const c = document.querySelector('#ggufContainer [data-backend-id]');
    if (c) c.click();
  });
  await page.waitForTimeout(3000);
  await page.evaluate(() => {
    const t = document.querySelector('.gguf-detail-tab[data-detail-tab="models"]');
    if (t) t.click();
  });
  await page.waitForTimeout(6000);

  const opened = await page.evaluate((model) => {
    const accs = Array.from(document.querySelectorAll('details.gguf-mp-acc'));
    for (const acc of accs) {
      const name = acc.getAttribute('data-model-acc') || '';
      if (name.indexOf(model.replace(/\.gguf$/, '')) !== -1) {
        acc.open = true;
        return { ok: true, name };
      }
    }
    return { ok: false, seen: accs.map(a => a.getAttribute('data-model-acc')) };
  }, MODEL);
  await page.waitForTimeout(1200);

  const set = await page.evaluate((want) => {
    const sel = document.querySelector('details.gguf-mp-acc[open] [data-field="kvCacheType"]');
    if (!sel) return { ok: false };
    sel.value = want;
    sel.dispatchEvent(new Event('change', { bubbles: true }));
    return { ok: true, value: sel.value };
  }, WANT);
  await page.waitForTimeout(600);

  const clicked = await page.evaluate(() => {
    const acc = document.querySelector('details.gguf-mp-acc[open]');
    if (!acc) return { ok: false };
    const btn = acc.querySelector('[data-model-params-apply]');
    if (!btn) return { ok: false, reason: 'no apply button' };
    btn.click();
    return { ok: true, model: btn.getAttribute('data-model-params-apply') };
  });

  // Ждём сохранения профиля и перезагрузки.
  await sleep(60000);
  const callsAfter = calls.slice();

  // Читаем факт напрямую у воркера через страницу (тот же origin + токен).
  const fact = await page.evaluate(async () => {
    const r = await fetch('/api/v1/gguf/backends', { headers: { 'X-CSRF-Token': '' } });
    const j = await r.json();
    const out = [];
    (j.backends || []).forEach(b => (b.models || []).forEach(m => out.push({
      name: m.name, status: m.status, kv: m.kv_cache_type, ctx: m.contextSize,
      gpu: m.gpu_layers, parallel: m.parallel
    })));
    return out;
  }).catch(e => 'fetch failed: ' + e.message);

  // И чипы карточки (после перерисовки).
  await page.evaluate(() => {
    const t = document.querySelector('.gguf-detail-tab[data-detail-tab="loaded"]');
    if (t) t.click();
  });
  await page.waitForTimeout(4000);
  const chips = await page.evaluate(() =>
    Array.from(document.querySelectorAll('.gguf-flag')).map(e => e.innerText.trim()));

  console.log(JSON.stringify({
    want: WANT,
    opened, set, clicked,
    apiCalls: callsAfter,
    facts: fact,
    chips,
    errors: errs.slice(0, 5)
  }, null, 1));

  await b.close();
})().catch(e => { console.log('FATAL ' + e.message); process.exit(1); });
