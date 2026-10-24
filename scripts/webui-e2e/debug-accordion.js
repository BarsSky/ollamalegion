const { chromium } = require('playwright');
// R207: проверка раскладки аккордеона параметров в карточке загруженной модели.
// Синтетическая разметка = реальная структура карточки (см. renderLoadedPane),
// поэтому проверяется именно CSS, а не доступ к данным (вход не нужен).
(async () => {
  const b = await chromium.launch({ channel: 'msedge', headless: true });
  const page = await b.newPage({ viewport: { width: 1400, height: 900 } });
  await page.goto('http://127.0.0.1:18083/index.html', { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(1500);
  await page.addStyleTag({ path: 'webui/css/pages.css' }).catch(() => {});
  await page.evaluate(() => {
    const card = document.createElement('div');
    card.className = 'gguf-loaded-model-item';
    card.id = 'r207card';
    card.innerHTML = '<div class="gguf-loaded-model-name">gemma-4-E4B-it-Q4_K_M</div>' +
      '<div class="gguf-loaded-model-info"><span>ctx: 8192</span></div>' +
      '<button class="btn btn-sm btn-danger">unload</button>' +
      '<details class="gguf-mp-acc"><summary class="gguf-mp-summary">params</summary>' +
      '<div class="gguf-mp-editor" style="height:120px">fields</div></details>';
    document.body.appendChild(card);
  });
  const box = await page.evaluate(() => {
    const card = document.getElementById('r207card');
    const acc = card.querySelector('.gguf-mp-acc');
    const name = card.querySelector('.gguf-loaded-model-name');
    const c = card.getBoundingClientRect();
    const a = acc.getBoundingClientRect();
    const n = name.getBoundingClientRect();
    return {
      card: { x: Math.round(c.x), y: Math.round(c.y), w: Math.round(c.width) },
      acc: { x: Math.round(a.x), y: Math.round(a.y), w: Math.round(a.width) },
      nameBottom: Math.round(n.bottom),
      below: a.y >= n.bottom,
      fullWidth: Math.abs(a.width - (c.width - 32)) < 6,
      sameRow: Math.abs(a.y - n.y) < 4
    };
  });
  console.log(JSON.stringify(box, null, 1));
  await b.close();
})().catch(e => console.log('FATAL ' + e.message));
