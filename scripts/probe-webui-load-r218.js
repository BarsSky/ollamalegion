// probe-webui-load-r218.js — R218: штатная загрузка/выгрузка модели ЧЕРЕЗ WebUI.
//
// Сценарий:
//  1. login (временный админ ui-test);
//  2. GGUF -> бэкенд -> вкладка «Модели»;
//  3. «Загрузить модель» на карточке gemma (кнопка, без правок формы);
//  4. ждём state=loaded, проверяем раскладку через API и VRAM донора (ssh);
//  5. «Выгрузить» на вкладке «Загружено»;
//  6. проверяем, что VRAM донора вернулась к базовой.
//
// Запуск: node scripts/probe-webui-load-r218.js
const { chromium } = require('playwright');
const { execSync } = require('child_process');
const http = require('http');

const WEBUI = 'http://127.0.0.1:18083/index.html';
const ADMIN = 'http://127.0.0.1:18081';
const TOKEN = 'changeme-bundled-with-agent-token';
const MODEL_ID = 'gemma-4-E4B-it-Q4_K_M.gguf';
const MODEL_NAME = 'gemma-4-E4B-it-Q4_K_M';

const sleep = ms => new Promise(r => setTimeout(r, ms));

function getJSON(port, path, token) {
  return new Promise((resolve, reject) => {
    const r = http.request({ host: '127.0.0.1', port, path, method: 'GET',
      headers: token ? { 'X-API-Token': token } : {} }, res => {
      let s = '';
      res.on('data', d => (s += d));
      res.on('end', () => { try { resolve(JSON.parse(s)); } catch (e) { resolve(s); } });
    });
    r.on('error', reject);
    r.end();
  });
}

function donorVram() {
  try {
    return execSync('ssh legion2 "docker exec cppworker-node2 nvidia-smi --query-gpu=memory.used --format=csv,noheader"',
      { encoding: 'utf8', maxBuffer: 4 << 20 }).trim();
  } catch (e) { return 'ERR'; }
}

function masterVram() {
  try {
    return execSync('docker exec ol-stack-cppworker-gpu nvidia-smi --query-gpu=memory.used --format=csv,noheader',
      { encoding: 'utf8', maxBuffer: 4 << 20 }).trim();
  } catch (e) { return 'ERR'; }
}

(async () => {
  console.log('VRAM до теста: master/donor =', masterVram(), '/', donorVram());

  const browser = await chromium.launch({ channel: 'msedge', headless: true });
  const page = await (await browser.newContext()).newPage();
  const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  const calls = [];
  page.on('response', async r => {
    const u = r.url();
    if (!/\/api\/(v1\/cppworker|models)/.test(u)) return;
    calls.push(r.request().method() + ' ' + u.replace('http://127.0.0.1:18083', '') + ' -> ' + r.status());
  });

  await page.goto(WEBUI, { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(2000);
  await page.fill('#authName', 'ui-test');
  await page.fill('#authPassword', 'Ui-Test-1');
  await page.click('#authSubmit');
  await page.waitForTimeout(3000);

  await page.click('.nav-item[data-page="gguf"]');
  await page.waitForTimeout(4000);
  await page.evaluate(() => {
    const c = document.querySelector('#ggufContainer [data-backend-id]');
    if (c) c.click();
  });
  await page.waitForTimeout(3000);
  await page.evaluate(() => {
    const t = document.querySelector('.gguf-detail-tab[data-detail-tab="models"]');
    if (t) t.click();
  });
  await page.waitForTimeout(5000);

  // Нажать «Загрузить модель» на карточке gemma.
  const clicked = await page.evaluate((modelId) => {
    const base = modelId.replace(/\.gguf$/, '');
    const cards = Array.from(document.querySelectorAll('.gguf-local-card'));
    for (const card of cards) {
      const text = card.textContent || '';
      if (text.indexOf(base) === -1) continue;
      const btn = card.querySelector('.gguf-load-btn');
      if (btn) { btn.click(); return { ok: true, cardText: text.trim().slice(0, 60) }; }
      return { ok: false, reason: 'no .gguf-load-btn in card', cardText: text.trim().slice(0, 80) };
    }
    return { ok: false, reason: 'card not found', cards: cards.map(c => (c.textContent || '').trim().slice(0, 40)) };
  }, MODEL_ID);
  console.log('click load:', JSON.stringify(clicked));

  // Ждём loaded (до 4 минут).
  let loaded = null;
  for (let i = 0; i < 120; i++) {
    await sleep(2000);
    const models = await getJSON(18092, '/api/models', TOKEN);
    const list = (models && models.models) || [];
    const m = list.find(x => (x.name || '').indexOf(MODEL_NAME) === 0);
    if (m && m.state === 'loaded') { loaded = m; break; }
    if (m && m.state === 'error') { loaded = m; break; }
  }
  console.log('state после клика:', loaded ? loaded.state : 'не дождались');
  if (loaded) {
    console.log('layers:', JSON.stringify(loaded.device_layers_exact));
    console.log('layout_degraded:', loaded.layout_degraded, loaded.layout_degraded_reason || '');
  }
  console.log('VRAM после загрузки: master/donor =', masterVram(), '/', donorVram());

  // Вкладка «Загружено» -> «Выгрузить».
  await page.evaluate(() => {
    const t = document.querySelector('.gguf-detail-tab[data-detail-tab="loaded"]');
    if (t) t.click();
  });
  await page.waitForTimeout(4000);
  const unloaded = await page.evaluate(() => {
    const btn = document.querySelector('[data-action="unload"], [data-unload-model], button');
    const all = Array.from(document.querySelectorAll('button'));
    const target = all.find(b => /выгруз|unload/i.test(b.textContent || '') && !b.disabled);
    if (target) { target.click(); return { ok: true, text: target.textContent.trim().slice(0, 30) }; }
    return { ok: false };
  });
  console.log('click unload:', JSON.stringify(unloaded));
  await sleep(8000);
  const after = await getJSON(18092, '/api/models', TOKEN);
  const still = ((after && after.models) || []).find(x => (x.name || '').indexOf(MODEL_NAME) === 0);
  console.log('после выгрузки state:', still ? still.state : 'нет модели');
  console.log('VRAM после выгрузки: master/donor =', masterVram(), '/', donorVram());
  console.log('ошибки страницы:', errors.slice(0, 3));
  console.log('API вызовы:', calls.slice(-8));

  await browser.close();
})().catch(e => { console.log('FATAL', e.message); process.exit(1); });
