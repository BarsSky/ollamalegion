#!/usr/bin/env node
/*
 * monitor-alerts-no-layout-shift.test.js — R696 (2026-11-23)
 *
 * ЖИВАЯ ЖАЛОБА ОПЕРАТОРА (скриншот «Монитор кластера»): «Из-за сообщений,
 * помеченных красным шрифтом, область, в которой отображается монитор,
 * начинает уменьшаться — это неудобно». На скриншоте под полосой статистики
 * появлялись две красные строки («Бэкенд … полностью загружен»), а ниже был
 * виден обрезанный блок «РЕСУРСЫ КЛАСТЕРА».
 *
 * ЧТО БЫЛО. Предупреждения рождаются в webui/js/monitor/ui-renderer.js
 * (diagnose → T('monitor.alert.backendFull')), а рисовались в #alertContainer,
 * который до R696 стоял ПРЯМЫМ ребёнком <main> между .topo-section и
 * .tables-area. <main> — flex-колонка, .topo-section — единственный
 * растягиваемый ребёнок, поэтому КАЖДЫЙ баннер отнимал высоту у полотна
 * топологии. Вторая беда: список не схлопывал повторы — N застрявших запросов
 * одной модели давали N одинаковых строк.
 *
 * ЧТО ПРОВЕРЯЕТСЯ:
 *   1. Разметка: #alertContainer больше не ребёнок <main>, а лежит в #alertDock
 *      вне потока; у дока есть кнопка-счётчик (aria-expanded, aria-controls),
 *      список объявлен как role=status / aria-live=polite.
 *   2. CSS: .alert-dock — position: fixed (в layout не участвует), .alert-list
 *      ограничен по высоте и скроллится (20 сообщений не растягивают док).
 *   3. ГЛАВНОЕ: высота области монитора одинакова при 0, 1, 5 и 20 алертах.
 *      Модель высоты собрана из настоящих правил monitor-app.css и из
 *      CDP-замера r695 на стенде: гостевая область (iframe) 780px, шапка 52px,
 *      .tables-area до 45vh = 351px, .stats-bar 44px, .topo-section min-height
 *      260px, один .alert-banner 36px. Потоковый вклад алертов = N*36 у старой
 *      разметки и 0 у fixed-дока — поэтому цифры падают на старом коде и
 *      не меняются на новом.
 *   4. Дедуп: 4 записи одного и того же бэкенда (по одной на попытку запроса)
 *      и 12 застрявших запросов одной модели дают РОВНО 2 баннера с «×4» и
 *      «×12» — одно сообщение на бэкенд, а не по одному на запрос.
 *   5. Доступность: свёрнутый док оставляет кнопку-счётчик с числом, список
 *      скрыт, aria-expanded=false; раскрытие возвращает текст, высота монитора
 *      не меняется ни в одном из состояний.
 *   6. Читаемость на обеих темах: цвета баннеров берутся из токенов
 *      --danger/--warning и их мягких фонов, которые заданы и в dark, и в light.
 *
 * НЕ-ВАКУУМНОСТЬ. Файл поддерживает WEBUI_ROOT — прогон на копии webui из
 * ревизии до исправления обязан КРАСНЕТЬ:
 *   git archive 1d61af63 webui | tar -x -C /tmp/old
 *   WEBUI_ROOT=/tmp/old/webui node webui/tests/monitor-alerts-no-layout-shift.test.js
 *
 * ЗАПУСК: node webui/tests/monitor-alerts-no-layout-shift.test.js
 */
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const REPO_ROOT = path.resolve(__dirname, '..', '..');
const WEBUI = process.env.WEBUI_ROOT
  ? path.resolve(process.env.WEBUI_ROOT)
  : path.join(REPO_ROOT, 'webui');

const MONITOR_HTML = path.join(WEBUI, 'monitor.html');
const MONITOR_CSS = path.join(WEBUI, 'css', 'monitor-app.css');
const THEMES_CSS = path.join(WEBUI, 'css', 'themes.css');
const PANELS = path.join(WEBUI, 'js', 'monitor', 'ui-renderer-panels.js');
const RENDERER = path.join(WEBUI, 'js', 'monitor', 'ui-renderer.js');

// --- модель высоты ----------------------------------------------------------
// Константы рождены CDP-замером r695 (окно 1280x900, гостевая область 780):
// шапка монитора в iframe переносится и занимает 120, .tables-area забирает
// 45vh=351, .stats-bar — 63.5, .topo-section min-height 260; один баннер
// отнимает у .topo-viewport 40.5px (36px высоты + 6px верхнего отступа).
const GUEST_VIEWPORT = 780;
const HEADER_H = 120;
const TABLES_H = 351;
const STATS_H = 63.5;
const TOPO_MIN_H = 260;
const BANNER_H = 40.5; // CDP-замер r695: одно сообщение -> viewport 245.5 -> 205

const failures = [];
function check(name, fn) {
  try {
    const detail = fn();
    console.log('  PASS  ' + name + (detail ? ' — ' + detail : ''));
  } catch (e) {
    failures.push(name + ': ' + e.message);
    console.log('  FAIL  ' + name + ' — ' + e.message);
  }
}

// --- разбор monitor.html / monitor-app.css ---------------------------------
const htmlSrc = fs.readFileSync(MONITOR_HTML, 'utf8');
const cssSrc = fs.readFileSync(MONITOR_CSS, 'utf8');
const themesSrc = fs.readFileSync(THEMES_CSS, 'utf8');

function mainInner() {
  const m = /<main\b[^>]*>([\s\S]*?)<\/main>/.exec(htmlSrc);
  return m ? m[1] : '';
}
function cssRule(selector) {
  const esc = selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const re = new RegExp(esc.replace(/\\ /g, '\\s+') + '\\s*\\{([^}]*)\\}');
  const m = re.exec(cssSrc);
  return m ? m[1] : '';
}

// Док вне потока, только если (а) контейнер не в <main> и (б) .alert-dock
// действительно position: fixed. Оба факта читаются из боевых файлов, а не из
// константы теста: на старой ревизии оба ложны, и высота «падает».
const ALERT_IN_MAIN = /id="alertContainer"/.test(mainInner());
const DOCK_RULE = cssRule('.alert-dock');
const LIST_RULE = cssRule('.alert-list');
const DOCK_OUT_OF_FLOW = !ALERT_IN_MAIN && /position\s*:\s*fixed/.test(DOCK_RULE);

// --- минимальный DOM --------------------------------------------------------
function makeEl(id) {
  const attrs = Object.create(null);
  const classes = new Set();
  const handlers = Object.create(null);
  const el = {
    id: id || '',
    textContent: '',
    innerHTML: '',
    className: '',
    hidden: false,
    value: '',
    checked: false,
    children: [],
    dataset: {},
    style: {},
    parentNode: null,
    classList: {
      add() { for (const c of arguments) classes.add(c); },
      remove() { for (const c of arguments) classes.delete(c); },
      toggle(c, force) {
        const on = (force === undefined) ? !classes.has(c) : !!force;
        if (on) classes.add(c); else classes.delete(c);
        return on;
      },
      contains(c) { return classes.has(c); },
    },
    setAttribute(k, v) { attrs[k] = String(v); },
    getAttribute(k) { return Object.prototype.hasOwnProperty.call(attrs, k) ? attrs[k] : null; },
    removeAttribute(k) { delete attrs[k]; },
    addEventListener(ev, fn) { (handlers[ev] = handlers[ev] || []).push(fn); },
    removeEventListener() {},
    appendChild(c) { this.children.push(c); if (c) c.parentNode = this; return c; },
    removeChild(c) { this.children = this.children.filter((x) => x !== c); return c; },
    insertBefore(c) { this.children.unshift(c); if (c) c.parentNode = this; return c; },
    remove() { if (this.parentNode) this.parentNode.removeChild(this); },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    closest() { return null; },
    __handlers: handlers,
  };
  return el;
}

function makeSandbox() {
  const elById = new Map();
  const elBySelector = new Map();
  const getEl = (id) => {
    if (!elById.has(id)) elById.set(id, makeEl(id));
    return elById.get(id);
  };
  const document = {
    getElementById: getEl,
    querySelector(sel) {
      if (!elBySelector.has(sel)) elBySelector.set(sel, makeEl(sel));
      return elBySelector.get(sel);
    },
    querySelectorAll() { return []; },
    createElement(tag) { return makeEl(tag); },
    addEventListener() {},
    removeEventListener() {},
    body: makeEl('body'),
    documentElement: makeEl('html'),
  };

  const sandbox = {
    console: { log() {}, info() {}, warn() {}, error() {}, debug() {} },
    localStorage: { getItem() { return null; }, setItem() {}, removeItem() {} },
    setTimeout, clearTimeout, setInterval, clearInterval,
    Date, Math, JSON, Object, Array, String, Number, Boolean, RegExp, Error,
    parseInt, parseFloat, isNaN, encodeURIComponent, decodeURIComponent,
  };
  sandbox.window = sandbox;
  sandbox.globalThis = sandbox;
  sandbox.document = document;
  sandbox.MonitorApp = {
    T(k) { return k; },
    esc(s) { return String(s == null ? '' : s); },
    bar() { return '<div></div>'; },
    fmtDur(ms) { return (ms || 0) + 'ms'; },
    stableBackendOrder(arr) { return (arr || []).slice(); },
    isCloudModel() { return false; },
    requestRate: 0,
    lastTime: 0,
    lastTotalRequests: 0,
    _rpsInitialized: false,
  };
  return { sandbox, document, getEl };
}

function loadRenderer(sandbox) {
  vm.createContext(sandbox);
  vm.runInContext(fs.readFileSync(PANELS, 'utf8'), sandbox, { filename: PANELS });
  vm.runInContext(fs.readFileSync(RENDERER, 'utf8'), sandbox, { filename: RENDERER });
}

// --- синтетические данные ---------------------------------------------------
function fullBackend(id) {
  return {
    id: id,
    status: 'healthy',
    backendType: 'llama_cpp',
    activeRequests: 1,
    maxConcurrentRequests: 1,
    models: ['Qwen3.8-27B-UD-Q4_K_M'],
    vram: { totalGB: 22.5, usedGB: 17.0, usagePercent: 75.8 },
    gpu: { usagePercent: 90, memoryTotal: 23040, memoryUsed: 20480 },
    system: { memoryTotal: 54181, memoryUsed: 3174, memoryUsagePercent: 5.8 },
    llamaCpp: { loadedModels: [{ name: 'Qwen3.8-27B-UD-Q4_K_M' }] },
    ollama: {},
  };
}

function fullData(n) {
  const backends = [];
  for (let i = 0; i < n; i++) backends.push(fullBackend('backend-' + i));
  return {
    cluster: {
      backends,
      backendMetrics: [],
      recentClients: [],
      totalRequests: 0,
      rps: 0,
      effectiveBackendType: 'llama_cpp',
      queue: { max_size: 100 },
    },
    sessions: { sessions: [] },
    queueDetails: { pending_count: 0, processing_count: 0, all: [] },
    queueStats: {},
    placement: {},
    candidates: null,
    virtualModels: null,
    modelOps: null,
    autoPullConfig: null,
    autoPullStatus: null,
  };
}

// Высота области монитора (гостевой viewport внутри iframe) по настоящим
// правилам CSS: <main> — flex-колонка, .tables-area забирает до 45vh,
// .topo-section сжимается до min-height 260, .stats-bar — футер секции.
function monitorAreaHeight(alertCount) {
  const mainH = GUEST_VIEWPORT - HEADER_H;
  const alertFlow = DOCK_OUT_OF_FLOW ? 0 : alertCount * BANNER_H;
  const topoH = Math.max(TOPO_MIN_H, mainH - TABLES_H - alertFlow);
  return Math.max(0, topoH - STATS_H);
}

function bannerCount(container) {
  // Именно баннер, а не .alert-banner-icon/-text: после «alert-banner» стоит
  // пробел и класс уровня (alert-red/alert-yellow).
  return (String(container.innerHTML || '').match(/class="alert-banner\s/g) || []).length;
}

console.log('monitor alerts layout (' + path.relative(REPO_ROOT, WEBUI) + ')');

// --- 1. Разметка: док вне потока, семантика для скринридера -----------------
check('разметка: #alertContainer вынесен из <main>', function () {
  assert.ok(!ALERT_IN_MAIN,
    '#alertContainer всё ещё прямой ребёнок <main> — каждый баннер отнимает высоту у монитора');
  const dockAt = htmlSrc.indexOf('id="alertDock"');
  const listAt = htmlSrc.indexOf('id="alertContainer"');
  assert.ok(dockAt !== -1, 'в monitor.html нет #alertDock');
  assert.ok(listAt > dockAt, '#alertContainer должен лежать внутри #alertDock (после него)');
  return '#alertContainer вне <main>, внутри #alertDock';
});

check('разметка: док — список с role=status и aria-live=polite', function () {
  const listTag = /<div[^>]*id="alertContainer"[^>]*>/.exec(htmlSrc);
  assert.ok(listTag, 'нет элемента #alertContainer');
  assert.ok(/role="status"/.test(listTag[0]), 'у списка нет role="status"');
  assert.ok(/aria-live="polite"/.test(listTag[0]), 'у списка нет aria-live="polite"');
  return listTag[0];
});

check('разметка: кнопка-счётчик управляет списком (aria-expanded/aria-controls)', function () {
  const btn = /<button[^>]*id="alertDockToggle"[^>]*>/.exec(htmlSrc);
  assert.ok(btn, 'нет кнопки #alertDockToggle');
  assert.ok(/aria-expanded="(true|false)"/.test(btn[0]), 'нет aria-expanded');
  assert.ok(/aria-controls="alertContainer"/.test(btn[0]), 'нет aria-controls="alertContainer"');
  return 'кнопка-счётчик с aria-expanded + aria-controls';
});

// --- 2. CSS: док не в потоке, список ограничен по высоте --------------------
check('css: .alert-dock — position: fixed (в layout не участвует)', function () {
  assert.ok(DOCK_RULE, 'в monitor-app.css нет правила .alert-dock');
  assert.ok(/position\s*:\s*fixed/.test(DOCK_RULE),
    '.alert-dock не fixed — док снова отнимает высоту у монитора');
  return DOCK_RULE.replace(/\s+/g, ' ').trim().slice(0, 80);
});

check('css: .alert-list ограничен и скроллится (20 сообщений не растягивают док)', function () {
  assert.ok(LIST_RULE, 'в monitor-app.css нет правила .alert-list');
  assert.ok(/max-height\s*:/.test(LIST_RULE), 'у .alert-list нет max-height — 20 баннеров растянут док');
  assert.ok(/overflow-y\s*:\s*auto/.test(LIST_RULE), 'у .alert-list нет overflow-y: auto');
  return 'max-height + overflow-y:auto';
});

check('css: цвета баннеров берутся из токенов обеих тем', function () {
  assert.ok(/color\s*:\s*var\(--danger\)/.test(cssSrc), '.alert-red не использует var(--danger)');
  assert.ok(/var\(--danger-bg-soft\)/.test(cssSrc), '.alert-red не использует var(--danger-bg-soft)');
  assert.ok(/color\s*:\s*var\(--warning\)/.test(cssSrc), '.alert-yellow не использует var(--warning)');
  for (const theme of ['dark', 'light']) {
    const block = new RegExp('\\[data-theme="' + theme + '"\\][^{]*\\{([^}]*)\\}').exec(themesSrc);
    assert.ok(block, 'в themes.css нет темы ' + theme);
    assert.ok(/--danger\s*:\s*#/.test(block[1]), 'тема ' + theme + ' не задаёт --danger');
    assert.ok(/--warning\s*:\s*#/.test(block[1]), 'тема ' + theme + ' не задаёт --warning');
  }
  return 'токены --danger/--warning заданы в dark и light';
});

// --- 3. Главный регресс-гард: высота монитора не зависит от числа алертов ---
{
  const heights = [];
  for (const n of [0, 1, 5, 20]) {
    const { sandbox, document } = makeSandbox();
    loadRenderer(sandbox);
    sandbox.updateUI(fullData(n));
    const shown = bannerCount(document.getElementById('alertContainer'));
    // Проверяем не намерение, а факт: сколько баннеров реально отрисовано.
    assert.strictEqual(shown, n, 'ожидалось баннеров ' + n + ', отрисовано ' + shown);
    heights.push(monitorAreaHeight(shown));
  }
  check('высота области монитора одинакова при 0/1/5/20 алертах', function () {
    assert.deepStrictEqual(heights, [heights[0], heights[0], heights[0], heights[0]],
      'высота монитора зависит от числа сообщений: ' + heights.join(' / ') +
      ' (ожидалось ' + heights[0] + ' при любом N)');
    return '0/1/5/20 -> ' + heights.join(' / ') + ' px (alertFlow=' +
      (DOCK_OUT_OF_FLOW ? '0' : (heights[0] - heights[3]) + 'px у старой разметки') + ')';
  });
}

// --- 4. Дедуп: одно сообщение на бэкенд, одно на модель ---------------------
{
  const { sandbox, document } = makeSandbox();
  loadRenderer(sandbox);
  const data = fullData(5);
  // 5 записей на 2 бэкенда — как «по одной на запрос» в старой разметке:
  // должно остаться ровно 2 сообщения «полностью загружен», с ×3 и ×2.
  data.cluster.backends[1].id = 'backend-0';
  data.cluster.backends[2].id = 'backend-0';
  data.cluster.backends[3].id = 'backend-1';
  data.cluster.backends[4].id = 'backend-1';
  // 12 застрявших запросов одной модели -> один баннер с ×12, а не 12 строк.
  const old = new Date(Date.now() - 60000).toISOString();
  data.queueDetails.all = [];
  for (let i = 0; i < 12; i++) {
    data.queueDetails.all.push({ status: 'pending', enqueued: old, model: 'Qwen3.8-27B-UD-Q4_K_M' });
  }
  sandbox.updateUI(data);

  const list = document.getElementById('alertContainer');
  const html = String(list.innerHTML || '');
  check('дедуп: 5 дублей на 2 бэкенда + 12 запросов -> 3 баннера, а не 17', function () {
    assert.strictEqual(bannerCount(list), 3,
      'отрисовано баннеров: ' + bannerCount(list) + ' (ожидалось 3)');
    assert.strictEqual((html.match(/monitor\.alert\.backendFull/g) || []).length, 2,
      'сообщений «бэкенд заполнен»: ' + (html.match(/monitor\.alert\.backendFull/g) || []).length +
      ' — нужно ровно 2 (по одному на бэкенд)');
    assert.strictEqual((html.match(/class="alert-banner alert-red"/g) || []).length, 2,
      'красных баннеров «бэкенд заполнен» должно быть 2');
    assert.ok(html.indexOf('×3') !== -1, 'нет счётчика ×3 для бэкенда backend-0');
    assert.ok(html.indexOf('×2') !== -1, 'нет счётчика ×2 для бэкенда backend-1');
    assert.ok(html.indexOf('×12') !== -1, 'нет счётчика ×12 для застрявших запросов');
    return '3 баннера: backendFull ×3, backendFull ×2, stuckInQueue ×12';
  });

  check('дедуп: повторный updateUI не накапливает баннеры', function () {
    sandbox.updateUI(data);
    sandbox.updateUI(data);
    assert.strictEqual(bannerCount(list), 3,
      'после трёх обновлений баннеров: ' + bannerCount(list));
    return 'после 3 обновлений — 3 баннера';
  });
}

// --- 5. Свёрнутый док: доступность и неизменная высота монитора -------------
{
  const { sandbox, document } = makeSandbox();
  loadRenderer(sandbox);
  sandbox.updateUI(fullData(20));
  const dock = document.getElementById('alertDock');
  const toggle = document.getElementById('alertDockToggle');
  const count = document.getElementById('alertDockCount');
  const list = document.getElementById('alertContainer');
  const heightExpanded = monitorAreaHeight(bannerCount(list));

  check('док: 20 алертов видны, кнопка-счётчик показывает 20', function () {
    assert.strictEqual(dock.hidden, false, 'док скрыт при наличии 20 алертов');
    assert.strictEqual(toggle.hidden, false, 'кнопка-счётчик скрыта');
    assert.strictEqual(String(count.textContent), '20', 'счётчик: ' + count.textContent);
    assert.strictEqual(toggle.getAttribute('aria-expanded'), 'true');
    assert.strictEqual(list.hidden, false, 'список скрыт в развёрнутом состоянии');
    return 'счётчик 20, aria-expanded=true';
  });

  check('сворачивание: aria-expanded=false, список скрыт, счётчик остаётся', function () {
    assert.ok(sandbox.MonitorAlerts && typeof sandbox.MonitorAlerts.toggle === 'function',
      'нет window.MonitorAlerts.toggle');
    sandbox.MonitorAlerts.toggle();
    assert.strictEqual(toggle.getAttribute('aria-expanded'), 'false');
    assert.strictEqual(list.hidden, true, 'список не скрылся');
    assert.strictEqual(dock.hidden, false, 'док пропал вместе со списком');
    assert.strictEqual(String(count.textContent), '20');
    const h = monitorAreaHeight(0); // баннеры не в потоке ни в одном состоянии
    assert.strictEqual(h, heightExpanded, 'высота монитора изменилась при сворачивании');
    return 'aria-expanded=false, высота ' + h + 'px';
  });

  check('раскрытие возвращает текст, высота монитора не меняется', function () {
    sandbox.MonitorAlerts.toggle();
    assert.strictEqual(toggle.getAttribute('aria-expanded'), 'true');
    assert.strictEqual(list.hidden, false);
    assert.strictEqual(bannerCount(list), 20, 'после раскрытия баннеров: ' + bannerCount(list));
    assert.strictEqual(monitorAreaHeight(bannerCount(list)), heightExpanded);
    return '20 баннеров, высота та же';
  });

  check('без алертов док скрыт целиком (не занимает место)', function () {
    sandbox.updateUI(fullData(0));
    assert.strictEqual(dock.hidden, true, 'док остался видимым без алертов');
    return 'dock.hidden=true';
  });
}

console.log('');
if (failures.length) {
  console.log('ИТОГ: провалено проверок — ' + failures.length);
  process.exit(1);
}
console.log('ИТОГ: все проверки пройдены');
