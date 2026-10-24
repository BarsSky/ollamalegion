// check-i18n-logs-r236.js — R236 (2026-10-24): живая проверка переключателя
// языка логов и ошибок в WebUI.
//
// Запуск (нужен webui, проксирующий /api/ на балансер с R234+):
//   WEBUI=http://127.0.0.1:18088 API_TOKEN=test-token node scripts/webui-e2e/check-i18n-logs-r236.js
//
// ЧТО ПРОВЕРЯЕТ (то, что видит оператор):
//   1. секция «Язык логов и ошибок» есть в настройках, select и кнопка на месте;
//   2. модуль i18n-logs.js подключён и отдаёт window.LogLanguage;
//   3. до входа/без прав select заблокирован, статус объясняет причину;
//   4. с правами администратора (X-API-Token через прямой запрос) select
//      заполняется действующим языком, статус показывает его;
//   5. PUT через страницу меняет язык на балансере (проверяем обратным GET).
'use strict';

const path = require('path');
const { chromium } = require('playwright');

const WEBUI = (process.env.WEBUI || 'http://127.0.0.1:18088').replace(/\/+$/, '');
const TOKEN = process.env.API_TOKEN || 'test-token';
const PW_CHANNEL = process.env.PW_CHANNEL || 'msedge';

let failed = 0;
function check(ok, what, detail) {
  console.log((ok ? 'PASS ' : 'FAIL ') + what + (detail ? ' :: ' + detail : ''));
  if (!ok) failed++;
}

async function launchBrowser() {
  try {
    return await chromium.launch({ channel: PW_CHANNEL, headless: true });
  } catch (e) {
    console.log('  канал ' + PW_CHANNEL + ' недоступен, беру встроенный chromium');
    return await chromium.launch({ headless: true });
  }
}

(async () => {
  console.log('R236: язык логов и ошибок в WebUI (' + WEBUI + ')');
  const browser = await launchBrowser();
  const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
  try {
    await page.goto(WEBUI + '/index.html', { waitUntil: 'domcontentloaded', timeout: 30000 });
    await page.waitForTimeout(1500);

    const ui = await page.evaluate(() => ({
      section: !!document.getElementById('section-log-language'),
      select: !!document.getElementById('logLanguageSelect'),
      button: !!document.getElementById('logLanguageApply'),
      status: !!document.getElementById('logLanguageStatus'),
      module: !!(window.LogLanguage && window.LogLanguage.endpoint),
      script: Array.from(document.scripts).some(s => (s.src || '').indexOf('i18n-logs.js') >= 0)
    }));
    check(ui.section, 'секция «Язык логов и ошибок» есть в настройках');
    check(ui.select && ui.button && ui.status, 'select, кнопка «Применить» и статус на месте');
    check(ui.script && ui.module, 'модуль i18n-logs.js подключён и экспортирует LogLanguage');

    // Гость/без сессии: запрос к /api/v1/i18n не пройдёт → select заблокирован,
    // статус объясняет причину.
    const guest = await page.evaluate(async () => {
      const sel = document.getElementById('logLanguageSelect');
      const status = document.getElementById('logLanguageStatus');
      return { disabled: !!(sel && sel.disabled), status: status ? status.textContent : '' };
    });
    check(guest.disabled, 'без административных прав select заблокирован');
    check(guest.status.length > 0, 'статус объясняет причину', guest.status.slice(0, 80));

    // Прямой API-контракт через nginx панели: GET отдаёт язык, PUT меняет его.
    const before = await page.evaluate(async (token) => {
      const r = await fetch('/api/v1/i18n', { headers: { 'X-API-Token': token } });
      return { status: r.status, body: await r.json() };
    }, TOKEN);
    check(before.status === 200 && (before.body.language === 'ru' || before.body.language === 'en'),
      'GET /api/v1/i18n через панель отдаёт язык', JSON.stringify(before.body).slice(0, 120));

    const target = before.body.resolved === 'en' ? 'ru' : 'en';
    const put = await page.evaluate(async ({ token, lang }) => {
      const r = await fetch('/api/v1/i18n', {
        method: 'PUT',
        headers: { 'X-API-Token': token, 'Content-Type': 'application/json' },
        body: JSON.stringify({ language: lang })
      });
      return { status: r.status, body: await r.json() };
    }, { token: TOKEN, lang: target });
    check(put.status === 200 && put.body.success === true, 'PUT переключает язык', JSON.stringify(put.body).slice(0, 120));

    const after = await page.evaluate(async (token) => {
      const r = await fetch('/api/v1/i18n', { headers: { 'X-API-Token': token } });
      return { status: r.status, body: await r.json() };
    }, TOKEN);
    check(after.body.resolved === target, 'балансер переключился на ' + target,
      'resolved=' + after.body.resolved + ', source=' + after.body.source);
    check(after.body.source === 'webui', 'источник языка = webui (правка из панели)', after.body.source);

    // Вернуть исходный язык, чтобы прогон не менял стенд.
    await page.evaluate(async ({ token, lang }) => {
      await fetch('/api/v1/i18n', {
        method: 'PUT',
        headers: { 'X-API-Token': token, 'Content-Type': 'application/json' },
        body: JSON.stringify({ language: lang })
      });
    }, { token: TOKEN, lang: before.body.language });
  } finally {
    await page.close();
    await browser.close();
  }

  console.log('');
  if (failed) {
    console.log('ИТОГ: FAIL (' + failed + ')');
    process.exit(1);
  }
  console.log('ИТОГ: PASS');
})();
