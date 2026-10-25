// check-demo-mode-r232.js — R232 (2026-10-24): живая проверка демо-режима WebUI.
//
// Запуск (нужен webui, поднятый с WEBUI_DEMO_MODE=1):
//   WEBUI=http://127.0.0.1:18084 node scripts/webui-e2e/check-demo-mode-r232.js
//
// Браузер: по умолчанию канал msedge (как и в остальных проверках scripts/webui-e2e):
// встроенный chromium/headless_shell на этой машине зависает на newPage. Переопределить
// можно переменной PW_CHANNEL (например PW_CHANNEL=chrome); если канал не найден —
// берётся встроенный chromium.
//
// ЧТО ПРОВЕРЯЕТ (то, что видит гость страницы):
//   1. DEMO_MODE включён в конфиге, вход НЕ требуется;
//   2. баннер «ДЕМО-РЕЖИМ» и панель «Демо-сценарий» видны;
//   3. сценарий живой: фаза меняется, клиенты появляются, счётчики обновляются;
//   4. данные синтетические: /api/v1/cluster отвечает демо-ответом (заголовок
//      X-OllamaLegion-Demo) с мастером и донором, мутации — 403 read-only,
//      и НИ ОДИН /api/-запрос не уходит на реальный сервер;
//   5. monitor.html тоже открывается в демо (без экрана входа/ошибок) и
//      показывает бэкенды.
//
// Код возврата: 0 — PASS, 1 — FAIL.
'use strict';

const path = require('path');
const fs = require('fs');
const { chromium } = require('playwright');

const WEBUI = (process.env.WEBUI || 'http://127.0.0.1:18084').replace(/\/+$/, '');
const OUTDIR = path.join(__dirname, '..', '..', '_diag');
const PW_CHANNEL = process.env.PW_CHANNEL || 'msedge';

// launchBrowser — msedge по умолчанию, встроенный chromium как запасной вариант.
async function launchBrowser() {
  try {
    return await chromium.launch({ channel: PW_CHANNEL, headless: true });
  } catch (e) {
    console.log('  канал ' + PW_CHANNEL + ' недоступен (' + e.message.split('\n')[0] + '), беру встроенный chromium');
    return await chromium.launch({ headless: true });
  }
}

let failed = 0;
function check(ok, what, detail) {
  console.log((ok ? 'PASS ' : 'FAIL ') + what + (detail ? ' :: ' + detail : ''));
  if (!ok) failed++;
}

(async () => {
  console.log('R232: демо-режим WebUI (' + WEBUI + ')');
  const browser = await launchBrowser();
  const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
  const realApi = [];
  page.on('request', req => {
    const u = req.url();
    if (u.indexOf(WEBUI) === 0 && u.indexOf('/api/') >= 0) { realApi.push(req.method() + ' ' + u.replace(WEBUI, '')); }
  });
  try {
    // ---- index.html ------------------------------------------------------
    page.on('pageerror', e => console.log('  [pageerror] ' + e.message));
    await page.goto(WEBUI + '/index.html', { waitUntil: 'domcontentloaded', timeout: 30000 });

    const cfgDemo = await page.evaluate(() => !!(window.WEBUI_CONFIG && window.WEBUI_CONFIG.DEMO_MODE));
    check(cfgDemo, 'DEMO_MODE=true в WEBUI_CONFIG');

    const authVisible = await page.evaluate(() => {
      const el = document.getElementById('authOverlay');
      return !!(el && el.offsetParent !== null && getComputedStyle(el).display !== 'none');
    });
    check(!authVisible, 'вход не требуется (экран входа скрыт)');

    const banner = await page.evaluate(() => {
      const el = document.getElementById('demoModeBanner');
      return el ? { text: el.textContent || '', shown: getComputedStyle(el).display !== 'none' } : null;
    });
    check(!!banner && banner.shown && /ДЕМО/i.test(banner.text), 'баннер ДЕМО-РЕЖИМ виден',
      banner ? banner.text.slice(0, 80) : 'нет элемента');

    const panel = await page.evaluate(() => {
      const el = document.getElementById('demoScenario');
      return el ? { shown: getComputedStyle(el).display !== 'none', text: el.textContent || '' } : null;
    });
    check(!!panel && panel.shown, 'панель «Демо-сценарий» видна');

    // Гость не должен видеть админских кнопок: role viewer применяется auth.js.
    const adminVisible = await page.evaluate(() => {
      const nodes = document.querySelectorAll('[data-admin-only]');
      for (const n of nodes) {
        if (n.offsetParent !== null && getComputedStyle(n).display !== 'none') { return true; }
      }
      return false;
    });
    check(!adminVisible, 'админские действия скрыты (гость = viewer)');

    // ---- живой сценарий --------------------------------------------------
    const phase1 = await page.evaluate(() => (document.getElementById('demoPhase') || {}).textContent || '');
    check(!!phase1, 'текущая фаза сценария отображается', phase1);

    // Ждём, пока в сценарии появятся клиенты и слои (цикл стартует с загрузки).
    const progressed = await page.waitForFunction(() => {
      const clients = document.getElementById('demoClients');
      const gpu = document.getElementById('demoGpu');
      return clients && gpu && Number(clients.textContent) > 0 && /\d+\/66/.test(gpu.textContent || '');
    }, { timeout: 45000 }).then(() => true).catch(() => false);
    const counters = await page.evaluate(() => ({
      clients: (document.getElementById('demoClients') || {}).textContent,
      queue: (document.getElementById('demoQueue') || {}).textContent,
      gpu: (document.getElementById('demoGpu') || {}).textContent,
      rpc: (document.getElementById('demoRpc') || {}).textContent,
      tune: (document.getElementById('demoTune') || {}).textContent,
      feed: (document.getElementById('demoFeed') || {}).textContent || ''
    }));
    check(progressed, 'сценарий живой: клиенты и раскладка появились', JSON.stringify(counters).slice(0, 160));
    check(/[1-9]/.test(String(counters.clients)), 'клиенты подключились', 'clients=' + counters.clients);
    check(/\/66/.test(String(counters.gpu)), 'раскладка слоёв видна', 'gpu=' + counters.gpu);
    check(/клиент|очеред|донор|AutoTune|запрос/i.test(counters.feed), 'лента событий наполняется');
    const phaseLater = await page.evaluate(() => (document.getElementById('demoPhase') || {}).textContent || '');
    check(phaseLater !== '' || phase1 !== '', 'фаза сценария меняется/отображается');

    // ---- API-поверхность демо -------------------------------------------
    const api = await page.evaluate(async () => {
      const cluster = await fetch('/api/v1/cluster');
      const body = await cluster.json();
      const headers = {};
      cluster.headers.forEach((v, k) => { headers[k.toLowerCase()] = v; });
      const put = await fetch('/api/v1/config', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: '{}' });
      const me = await (await fetch('/api/v1/auth/me')).json();
      return {
        clusterStatus: cluster.status,
        demoHeader: headers['x-ollamalegion-demo'],
        backends: (body.backends || []).map(b => ({ id: b.id, role: b.role, status: b.status })),
        putStatus: put.status,
        meRole: me && me.user && me.user.role,
        meAuth: !!(me && me.authenticated)
      };
    });
    check(api.clusterStatus === 200 && api.demoHeader === '1', '/api/v1/cluster — демо-ответ');
    check(api.backends.length === 3, 'в демо три бэкенда (мастер, донор, image)', JSON.stringify(api.backends));
    check(api.backends.some(b => b.role === 'donor'), 'в демо есть донор (RPC)');
    check(api.putStatus === 403, 'мутации в демо запрещены (403 read-only)', 'PUT status=' + api.putStatus);
    check(api.meAuth === true && api.meRole === 'viewer', 'гостевая сессия viewer');
    check(realApi.length === 0, 'ни один /api/-запрос не ушёл на реальный сервер', JSON.stringify(realApi.slice(0, 5)));
  } finally {
    // скриншот — для разбора, если что-то не так
    try {
      if (!fs.existsSync(OUTDIR)) { fs.mkdirSync(OUTDIR, { recursive: true }); }
      await page.screenshot({ path: path.join(OUTDIR, 'demo-r232.png'), fullPage: false });
    } catch (e) { /* ignore */ }
    await page.close();

    // ---- monitor.html ----------------------------------------------------
    const monitor = await browser.newPage({ viewport: { width: 1600, height: 1000 } });
    const monitorRealApi = [];
    monitor.on('request', req => {
      const u = req.url();
      if (u.indexOf(WEBUI) === 0 && u.indexOf('/api/') >= 0) { monitorRealApi.push(req.method() + ' ' + u.replace(WEBUI, '')); }
    });
    try {
      await monitor.goto(WEBUI + '/monitor.html', { waitUntil: 'domcontentloaded', timeout: 30000 });
      const m = await monitor.evaluate(() => ({
        banner: !!document.getElementById('demoModeBanner'),
        panel: !!document.getElementById('demoScenario'),
        errorOverlay: (() => { const e = document.getElementById('errorOverlay'); return !!(e && e.offsetParent !== null); })(),
        backends: document.querySelectorAll('#backendsTable tbody tr').length
      }));
      check(m.banner && m.panel, 'monitor.html: баннер и панель демо видны');
      check(!m.errorOverlay, 'monitor.html: нет оверлея ошибки');
      check(m.backends >= 2, 'monitor.html: бэкенды отрисованы из демо-данных', 'rows=' + m.backends);
      check(monitorRealApi.length === 0, 'monitor.html: /api/-запросы не уходят на сервер', JSON.stringify(monitorRealApi.slice(0, 5)));

      // ---- R238: «живые» цифры в мониторе --------------------------------
      // До R238 у синтетических бэкендов не было activeRequests, поэтому Active
      // всегда был 0 (жалоба оператора). Сценарий начинает обработку на ~14 с,
      // поэтому ждём появления активного запроса.
      const sawActive = await monitor.waitForFunction(() => {
        const el = document.getElementById('statActive');
        return el && Number(el.textContent) > 0;
      }, { timeout: 60000 }).then(() => true).catch(() => false);
      const activeNow = await monitor.evaluate(() => ({
        active: (document.getElementById('statActive') || {}).textContent,
        rps: (document.getElementById('statRPSCluster') || {}).textContent,
        backends: (window.MonitorApp && window.MonitorApp.topo.backends || []).map(b => ({ id: b.id, a: b.activeRequests, m: b.maxConcurrentRequests }))
      }));
      check(sawActive, 'monitor: поле Active перестало быть всегда 0', JSON.stringify(activeNow));
      check(activeNow.backends.some(b => b.a > 0), 'monitor: у бэкенда есть activeRequests > 0');

      // ---- R238: конвейер рисует ОТВЕТ и все частицы подписаны -----------
      const sawResponse = await monitor.waitForFunction(() => {
        const MA = window.MonitorApp;
        if (!MA || !MA.convQueue) return false;
        return MA.convQueue.some(p => p.type === 'response' && p.label);
      }, { timeout: 90000 }).then(() => true).catch(() => false);
      const particles = await monitor.evaluate(() => {
        const MA = window.MonitorApp || {};
        const q = MA.convQueue || [];
        const resp = q.filter(p => p.type === 'response');
        return {
          demoLifecycle: !!MA.topo.demoLifecycle,
          total: q.length,
          responses: resp.length,
          labels: q.filter(p => !!p.label).length,
          sample: (resp[0] && resp[0].label) || (q[0] && q[0].label) || null,
          recentResponses: (MA.topo.recentResponses || []).length
        };
      });
      check(sawResponse, 'monitor: конвейер рисует частицу ОТВЕТА backend → клиент', JSON.stringify(particles));
      check(particles.demoLifecycle, 'monitor: включён «фактовый» режим конвейера (demoLifecycle)');
      check(particles.total > 0 && particles.labels === particles.total,
        'monitor: у ВСЕХ частиц конвейера есть подпись', JSON.stringify(particles));
    } finally {
      await monitor.close();
    }

    // ---- R238: мобильная вёрстка ----------------------------------------
    // Жалоба: «на смартфоне все элементы накладываются кучей». Проверяем
    // измеримые свойства: документ не шире экрана, шапка не выпирает, бургер
    // не под баннером, панель сценария свёрнута, а топология не наезжает на
    // балансер.
    const MOBILE = { width: 390, height: 844 };
    const mob = await browser.newPage({ viewport: MOBILE });
    try {
      await mob.goto(WEBUI + '/index.html', { waitUntil: 'domcontentloaded', timeout: 30000 });
      await mob.waitForFunction(() => !!document.getElementById('demoModeBanner'), { timeout: 15000 }).catch(() => {});
      const dash = await mob.evaluate(() => {
        const burger = document.querySelector('.nav-burger');
        const br = burger ? burger.getBoundingClientRect() : null;
        const panel = document.getElementById('demoScenario');
        const actions = document.querySelector('.header-actions');
        const ar = actions ? actions.getBoundingClientRect() : null;
        const banner = document.getElementById('demoModeBanner');
        return {
          vw: window.innerWidth,
          docScrollWidth: document.documentElement.scrollWidth,
          bannerH: banner ? Math.round(banner.getBoundingClientRect().height) : 0,
          burgerTop: br ? Math.round(br.top) : null,
          burgerShown: burger ? getComputedStyle(burger).display !== 'none' : false,
          actionsRight: ar ? Math.round(ar.right) : null,
          collapsed: !!(panel && panel.classList.contains('collapsed'))
        };
      });
      check(dash.docScrollWidth <= MOBILE.width + 1, 'телефон: страница не шире экрана',
        'scrollWidth=' + dash.docScrollWidth + ' vw=' + dash.vw);
      check(dash.actionsRight !== null && dash.actionsRight <= MOBILE.width + 1,
        'телефон: кнопки шапки не выпирают за экран', 'right=' + dash.actionsRight);
      check(dash.burgerShown && dash.burgerTop !== null && dash.burgerTop > dash.bannerH,
        'телефон: бургер под демо-баннером, а не под ним', JSON.stringify(dash));
      check(dash.collapsed, 'телефон: панель «Демо-сценарий» свёрнута по умолчанию');

      // Монитор внутри iframe не должен рисовать ВТОРОЙ баннер/панель/чип гостя.
      await mob.evaluate(() => { const n = document.querySelector('.nav-item[data-page=monitor]'); if (n) n.click(); });
      await mob.waitForTimeout(2500);
      const iframe = await mob.evaluate(() => {
        const f = document.getElementById('monitorFrame');
        try {
          const d = f.contentDocument;
          return {
            outer: document.querySelectorAll('#demoModeBanner').length,
            innerBanner: !!d.getElementById('demoModeBanner'),
            innerPanel: !!d.getElementById('demoScenario'),
            innerChip: (() => { const c = d.getElementById('authUserChip'); return !!(c && getComputedStyle(c).display !== 'none'); })()
          };
        } catch (e) { return { err: e.message }; }
      });
      check(iframe.outer === 1 && iframe.innerBanner === false && iframe.innerPanel === false,
        'телефон: в iframe монитора нет дубля баннера/панели демо', JSON.stringify(iframe));
      check(iframe.innerChip === false, 'телефон: в iframe монитора нет дубля чипа гостя');

      // Сама топология: колонки не наезжают на блок балансера.
      await mob.goto(WEBUI + '/monitor.html', { waitUntil: 'domcontentloaded', timeout: 30000 });
      await mob.waitForFunction(() => {
        const MA = window.MonitorApp;
        return MA && MA.topo && MA.topo.w > 0;
      }, { timeout: 20000 }).catch(() => {});
      const mon = await mob.evaluate(() => {
        const MA = window.MonitorApp || {};
        const G = MA.GEOM || {};
        const w = (MA.topo || {}).w || 0;
        const bar = document.querySelector('.stats-bar');
        const r = bar ? bar.getBoundingClientRect() : null;
        return {
          vw: window.innerWidth,
          docScrollWidth: document.documentElement.scrollWidth,
          stats: bar ? { sw: bar.scrollWidth, cw: bar.clientWidth, h: r ? Math.round(r.height) : 0 } : null,
          w: w,
          clientX: G.clientX,
          balInX: G.balInX ? G.balInX(w) : null,
          balOutX: G.balOutX ? G.balOutX(w) : null,
          backendX: G.backendX ? G.backendX(w) : null,
          bw: G.bw
        };
      });
      check(mon.docScrollWidth <= MOBILE.width + 1, 'телефон/монитор: страница не шире экрана',
        'scrollWidth=' + mon.docScrollWidth);
      check(!!mon.stats && mon.stats.sw <= mon.stats.cw + 1, 'телефон/монитор: полоса статистики не обрезается',
        JSON.stringify(mon.stats));
      check(mon.clientX <= mon.balInX - 8, 'телефон/монитор: клиентская ветка левее балансера',
        'clientX=' + mon.clientX + ' balInX=' + mon.balInX);
      check(mon.backendX >= mon.balOutX + 8, 'телефон/монитор: плитки бэкендов правее балансера',
        'backendX=' + mon.backendX + ' balOutX=' + mon.balOutX);
    } finally {
      await mob.close();
    }
    await browser.close();
  }

  console.log('');
  if (failed) {
    console.log('ИТОГ: FAIL (' + failed + ')');
    process.exit(1);
  }
  console.log('ИТОГ: PASS');
})();
