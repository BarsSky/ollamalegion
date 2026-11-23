#!/usr/bin/env node
/*
 * monitor-gate-banner-r700.test.js — R700 (2026-11-24)
 *
 * ЗАЧЕМ. На «Мониторе» оператор ищет причину простоя строкой предупреждения.
 * До R700 гейты (диск / RAM / VRAM / окно / слоты) туда не попадали: данные
 * лежали в /api/v1/queue/stats → gates.notices, но diagnose() их не читал, и
 * свободный-but-заблокированный узел выглядел как «балансер не использует
 * свободный бэкенд» — ровно живая жалоба второго стенда (`free_disk_mb=9480 <
 * min=10240`).
 *
 * ЧТО ПРОВЕРЯЕТСЯ:
 *   1. баннер причины появляется в доке предупреждений и содержит ЧИСЛА
 *      (9480/10240, 65536/131072) и ИМЯ УЗЛА;
 *   2. причина «критическая» (RAM) рисуется красной, остальные — жёлтыми;
 *   3. повторный updateUI не накапливает баннеры (дедуп по ключу гейт+узел);
 *   4. нет gates — нет баннеров (монитор не выдумывает тревог).
 *
 * НЕ-ВАКУУМНОСТЬ (проверено вручную, хук ниже):
 *   git show 0b5656b6:webui/js/monitor/ui-renderer.js > /tmp/old-ui-renderer.js
 *   MONITOR_RENDERER_TARGET=/tmp/old-ui-renderer.js node webui/tests/monitor-gate-banner-r700.test.js
 *   → КРАСНЕЕТ: старый diagnose() не читает queueStats.gates.
 *
 * ЗАПУСК: node webui/tests/monitor-gate-banner-r700.test.js
 */
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const REPO_ROOT = path.resolve(__dirname, '..', '..');
const MONITOR_DIR = path.join(REPO_ROOT, 'webui', 'js', 'monitor');
const PANELS = path.join(MONITOR_DIR, 'ui-renderer-panels.js');
const RENDERER = process.env.MONITOR_RENDERER_TARGET
  ? path.resolve(process.env.MONITOR_RENDERER_TARGET)
  : path.join(MONITOR_DIR, 'ui-renderer.js');

let failures = 0;
function check(name, fn) {
  try {
    fn();
    console.log('  ✓ ' + name);
  } catch (e) {
    failures++;
    console.error('  ✗ ' + name + ' — ' + (e && e.message));
  }
}
function assert(cond, msg) {
  if (!cond) throw new Error(msg);
}

// --- минимальный DOM (тот же приём, что в monitor-alerts-no-layout-shift) -----
function makeEl(id) {
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
      _c: {},
      add(c) { this._c[c] = true; },
      remove(c) { delete this._c[c]; },
      toggle(c, force) { const on = force === undefined ? !this._c[c] : !!force; if (on) this._c[c] = true; else delete this._c[c]; return on; },
      contains(c) { return !!this._c[c]; }
    },
    setAttribute() {},
    getAttribute() { return null; },
    removeAttribute() {},
    addEventListener(ev, fn) { (handlers[ev] = handlers[ev] || []).push(fn); },
    removeEventListener() {},
    appendChild(c) { this.children.push(c); if (c) c.parentNode = this; return c; },
    removeChild(c) { this.children = this.children.filter((x) => x !== c); return c; },
    insertBefore(c) { this.children.unshift(c); if (c) c.parentNode = this; return c; },
    remove() { if (this.parentNode) this.parentNode.removeChild(this); },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    closest() { return null; }
  };
  return el;
}

const elById = new Map();
const elBySelector = new Map();
function getEl(id) {
  if (!elById.has(id)) elById.set(id, makeEl(id));
  return elById.get(id);
}
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
  documentElement: makeEl('html')
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
  T(k, vars) {
    // Мини-перевод только того, что нужно этому тесту: остальные ключи
    // возвращаются как есть (проверяем именно текст причины, а не подписи).
    const m = {
      'monitor.alerts.merged': 'объединено: {count}',
      'monitor.alerts.toggle': 'свернуть',
      'monitor.alert.rebalance': 'перераспределить',
      'monitor.alert.windowAlignButton': 'выровнять',
    };
    let s = m[k] !== undefined ? m[k] : k;
    if (vars) Object.keys(vars).forEach(function (n) { s = s.split('{' + n + '}').join(String(vars[n])); });
    return s;
  },
  esc(s) { return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'); },
  bar() { return '<div></div>'; },
  fmtDur(ms) { return (ms || 0) + 'ms'; },
  stableBackendOrder(arr) { return (arr || []).slice(); },
  isCloudModel() { return false; },
  requestRate: 0,
  lastTime: 0,
  lastTotalRequests: 0,
  _rpsInitialized: false,
};

vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(PANELS, 'utf8'), sandbox, { filename: PANELS });
vm.runInContext(fs.readFileSync(RENDERER, 'utf8'), sandbox, { filename: RENDERER });

// --- данные: свободный узел, заблокированный диском, + RAM-критика ----------
function dataWithGates(notices) {
  return {
    cluster: {
      backends: [{
        id: 'A10-126', status: 'healthy', backendType: 'llama_cpp',
        activeRequests: 0, maxConcurrentRequests: 1,
        models: ['Qwen3.8-27B-UD-Q4_K_M'],
        vram: { totalGB: 22.5, usedGB: 17.0, usagePercent: 75.8 },
        gpu: { usagePercent: 90, memoryTotal: 23040, memoryUsed: 20480 },
        system: { memoryTotal: 54181, memoryUsed: 3174, memoryUsagePercent: 5.8 },
        llamaCpp: { loadedModels: [{ name: 'Qwen3.8-27B-UD-Q4_K_M' }] },
        ollama: {},
      }],
      backendMetrics: [],
      recentClients: [],
      totalRequests: 0,
      rps: 0,
      effectiveBackendType: 'llama_cpp',
      queue: { max_size: 100 },
    },
    sessions: { sessions: [] },
    queueDetails: { pending_count: 0, processing_count: 0, all: [] },
    queueStats: { gates: notices ? { notices: notices } : undefined },
    placement: {},
    candidates: null,
    virtualModels: null,
    modelOps: null,
    autoPullConfig: null,
    autoPullStatus: null,
  };
}

const diskNotice = {
  gate: 'disk',
  backend_id: 'A10-126',
  severity: 'warning',
  message: 'Запрос ждёт: свободный узел A10-126 заблокирован по диску (свободно 9480 МБ, минимум 10240 МБ) — освободите место или снизьте порог в настройках',
  count: 4,
};
const ramNotice = {
  gate: 'ram',
  backend_id: 'A10-127',
  severity: 'critical',
  message: 'Запрос ждёт: узел A10-127 исключён из выбора — RAM занята 96% (порог 85%)',
  count: 1,
};

function bannerHtml() {
  return String(getEl('alertContainer').innerHTML || '');
}
function bannerCount() {
  return (bannerHtml().match(/class="alert-banner\s/g) || []).length;
}

// ===========================================================================
check('баннер причины с числами и узлом появляется в доке монитора', function () {
  sandbox.updateUI(dataWithGates([diskNotice, ramNotice]));
  const html = bannerHtml();
  assert(html.indexOf('9480') !== -1 && html.indexOf('10240') !== -1,
    'в баннере нет чисел дисковой причины (9480/10240): ' + html);
  assert(html.indexOf('A10-126') !== -1, 'в баннере не назван узел A10-126: ' + html);
  assert(html.indexOf('96%') !== -1 && html.indexOf('85%') !== -1,
    'в баннере нет чисел RAM-причины: ' + html);
  assert(bannerCount() >= 2, 'ожидалось не меньше двух баннеров причин, got ' + bannerCount());
});

check('критическая причина красная, предупреждение — жёлтое', function () {
  sandbox.updateUI(dataWithGates([diskNotice, ramNotice]));
  const html = bannerHtml();
  assert(/alert-red[^>]*>[\s\S]*?96%/.test(html) || html.indexOf('alert-red') !== -1,
    'нет красного баннера для RAM-критики: ' + html);
  assert(html.indexOf('alert-yellow') !== -1, 'нет жёлтого баннера для disk-warning: ' + html);
});

check('повторный updateUI не накапливает баннеры (дедуп ключ гейт+узел)', function () {
  sandbox.updateUI(dataWithGates([diskNotice, ramNotice]));
  const first = bannerCount();
  sandbox.updateUI(dataWithGates([diskNotice, ramNotice]));
  sandbox.updateUI(dataWithGates([diskNotice, ramNotice]));
  assert(bannerCount() === first,
    'после трёх обновлений баннеров ' + bannerCount() + ' вместо ' + first + ': причина размножается на каждом опросе');
});

check('нет gates — нет баннеров причин', function () {
  sandbox.updateUI(dataWithGates(null));
  assert(bannerHtml().indexOf('Запрос ждёт') === -1,
    'баннер причины нарисован без данных gates: ' + bannerHtml());
});

// ===========================================================================
if (failures > 0) {
  console.error('\nFAIL: ' + failures + ' проверок красные (файл: ' + RENDERER + ')');
  process.exit(1);
}
console.log('\nOK: причины ожидания видны на мониторе баннером с числами');
