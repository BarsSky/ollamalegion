// debug-gguf-kv-chip-r213.js — R213 (2026-10-21).
//
// Проверяет, что блок «действующие параметры» загруженной модели показывает
// РЕАЛЬНЫЙ тип KV-cache (q4_0), а не догадку f16, и что слои на GPU берутся из
// фактов/раскладки, а не равны нулю.
//
// Запуск: node scripts/webui-e2e/debug-gguf-kv-chip-r213.js
const { chromium } = require('playwright');

(async () => {
  const b = await chromium.launch({ channel: 'msedge', headless: true });
  const page = await (await b.newContext()).newPage();
  const errs = [];
  page.on('pageerror', e => errs.push(e.message));

  await page.goto('http://127.0.0.1:18083/index.html', { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(2000);
  await page.fill('#authName', 'ui-check');
  await page.fill('#authPassword', 'Ui-Check-1');
  await page.click('#authSubmit');
  await page.waitForTimeout(3500);

  await page.click('.nav-item[data-page="gguf"]');
  await page.waitForTimeout(4500);

  const pickedBackend = await page.evaluate(() => {
    const card = document.querySelector('#ggufContainer [data-backend-id]');
    if (!card) return null;
    card.click();
    return card.getAttribute('data-backend-id');
  });
  await page.waitForTimeout(3500);
  await page.evaluate(() => {
    const t = document.querySelector('.gguf-detail-tab[data-detail-tab="models"]');
    if (t) t.click();
  });
  await page.waitForTimeout(4500);

  // Раскрыть карточку загруженной модели.
  await page.evaluate(() => {
    const acc = document.querySelector('details.gguf-mp-acc');
    if (acc) acc.open = true;
  });
  await page.waitForTimeout(1500);

  // Чипы «действующих параметров» — из блока загруженной модели.
  const flags = await page.evaluate(() => {
    const out = [];
    document.querySelectorAll('[class*="flag"], [class*="chip"]').forEach(e => {
      const text = (e.innerText || '').replace(/\s+/g, ' ').trim();
      if (!text || text.length > 120) return;
      out.push({ text, title: (e.getAttribute('title') || '').slice(0, 200), cls: e.className });
    });
    return out.slice(0, 60);
  });

  const kvFlags = flags.filter(f => /(^|\s)kv(\s|:|=|$)/i.test(f.text));
  const gpuFlags = flags.filter(f => /gpu|сло/i.test(f.text));
  const pageText = (await page.content()) || '';

  console.log(JSON.stringify({
    pickedBackend,
    kvFlags,
    gpuFlags: gpuFlags.slice(0, 6),
    f16RenderedAsKv: kvFlags.some(f => /f16/i.test(f.text)),
    f16Anywhere: /kv\s*:?\s*f16/i.test(pageText),
    errors: errs.slice(0, 5)
  }, null, 1));

  await b.close();
})().catch(e => { console.log('FATAL ' + e.message); process.exit(1); });
