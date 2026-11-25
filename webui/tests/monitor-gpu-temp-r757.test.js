#!/usr/bin/env node
/*
 * monitor-gpu-temp-r757.test.js — R757 (2026-11-25)
 *
 * ЗАЧЕМ. Живая жалоба оператора: «в балансере постоянно фигурировала информация
 * о возможном троттлинге из-за температуры выше пороговой, но явно об этом не
 * сообщалось в WebUI и не понятно, какая на самом деле температура критична для
 * видеокарты».
 *
 * До R757 в таблице бэкендов были только GPU% / VRAM% / CPU% / RAM%, а
 * температура и порог жили ИСКЛЮЧИТЕЛЬНО в логе балансера
 * (`backend GPU горячее заданного предела (soft, не блокирует)
 * temperature=88 threshold=85` — 14 повторов в архиве второго стенда).
 *
 * ЧТО ПРОВЕРЯЕТ:
 *   1. температура GPU рисуется в строке бэкенда;
 *   2. подсветка соответствует порогу из состояния кластера (выше порога -
 *      красный, в пределах 10 °C до порога - жёлтый, далеко - нейтральный);
 *   3. thermal-троттлинг из NVML виден даже когда температура ниже порога
 *      (это факт снижения частот, а не предсказание);
 *   4. в доке предупреждений есть баннер «горячее порога» с числами;
 *   5. при фактическом троттлинге баннер другой (причины от карты);
 *   6. без перегрева ложных баннеров нет;
 *   7. порог 0 (не задан) не ломает рендер: температура без оценки.
 *
 * НЕ-ВАКУУМНОСТЬ:
 *   git show HEAD:webui/js/monitor/ui-renderer.js > /tmp/old-renderer.js
 *   UI_RENDERER_TARGET=/tmp/old-renderer.js node webui/tests/monitor-gpu-temp-r757.test.js
 *   → КРАСНЕЕТ: в старом рендере температуры в таблице и баннеров нет.
 *
 * ЗАПУСК: node webui/tests/monitor-gpu-temp-r757.test.js
 */
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const REPO_ROOT = path.resolve(__dirname, '..', '..');
const TARGET = process.env.UI_RENDERER_TARGET
  ? path.resolve(process.env.UI_RENDERER_TARGET)
  : path.join(REPO_ROOT, 'webui', 'js', 'monitor', 'ui-renderer.js');

let failures = 0;
function check(name, ok, detail) {
  if (ok) {
    console.log('  ✓ ' + name);
  } else {
    failures++;
    console.log('  ✗ ' + name + (detail ? ' — ' + detail : ''));
  }
}

function fakeEl(id) {
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
    classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
    setAttribute() {},
    removeAttribute() {},
    getAttribute() { return null; },
    appendChild() {},
    removeChild() {},
    insertAdjacentHTML() {},
    addEventListener() {},
    removeEventListener() {},
    querySelector() { return null; },
    querySelectorAll() { return []; },
    closest() { return null; },
  };
  el.parentNode = { removeChild() {}, appendChild() {} };
  return el;
}

function makeSandbox() {
  const calls = { topology: 0 };
  const consoleErrors = [];
  const sandbox = {
    console: {
      log() {}, info() {}, warn() {}, debug() {},
      error: function () { consoleErrors.push(Array.prototype.slice.call(arguments).join(' ')); },
    },
    localStorage: {
      getItem() { return null; },
      setItem() {},
      removeItem() {},
    },
    setTimeout, clearTimeout, setInterval, clearInterval,
    Date, Math, JSON, Object, Array, String, Number, Boolean, RegExp, Error,
    parseInt, parseFloat, isNaN, encodeURIComponent, decodeURIComponent,
  };
  sandbox.window = sandbox;
  sandbox.globalThis = sandbox;

  const elById = new Map();
  const elBySelector = new Map();
  sandbox.document = {
    getElementById(id) {
      if (!elById.has(id)) elById.set(id, fakeEl(id));
      return elById.get(id);
    },
    querySelector(sel) {
      if (!elBySelector.has(sel)) elBySelector.set(sel, fakeEl(sel));
      return elBySelector.get(sel);
    },
    querySelectorAll() { return []; },
    createElement(tag) { return fakeEl(tag); },
    addEventListener() {},
    removeEventListener() {},
  };
  sandbox.document.body = fakeEl('body');
  sandbox.document.documentElement = fakeEl('html');

  sandbox.MonitorApp = {
    // T отдаёт ключ и подставленные значения: проверки баннеров должны видеть
    // ЧИСЛА (узел/температуру/порог/причину), а не только имя ключа.
    T: function (k, p) {
      var s = String(k);
      if (p) Object.keys(p).forEach(function (n) { s += ' ' + n + '=' + p[n]; });
      return s;
    },
    esc: function (s) { return String(s == null ? '' : s); },
    bar: function () { return '<div></div>'; },
    fmtDur: function (ms) { return (ms || 0) + 'ms'; },
    stableBackendOrder: function (arr) { return (arr || []).slice(); },
    isCloudModel: function () { return false; },
    requestRate: 0,
    lastTime: 0,
    lastTotalRequests: 0,
    _rpsInitialized: false,
  };
  sandbox.updateTopology = function () { calls.topology++; };

  return { sandbox, calls, consoleErrors };
}

function loadRenderer(sandbox) {
  const src = fs.readFileSync(TARGET, 'utf8');
  vm.createContext(sandbox);
  const panelsPath = path.join(REPO_ROOT, 'webui', 'js', 'monitor', 'ui-renderer-panels.js');
  vm.runInContext(fs.readFileSync(panelsPath, 'utf8'), sandbox, { filename: panelsPath });
  vm.runInContext(src, sandbox, { filename: TARGET });
}

// Один llama.cpp-бэкенд A10-126 — та самая карта из архива жалобы.
function sampleData(gpu, gpuMaxTemp) {
  return {
    cluster: {
      backends: [{
        id: 'A10-126',
        status: 'healthy',
        backendType: 'llama_cpp',
        activeRequests: 0,
        maxConcurrentRequests: 10,
        models: ['Inkling-2-Mini-UD-Q4_K_M'],
        vram: { totalGB: 22.5, usedGB: 17.0, usagePercent: 75.8 },
        gpu: gpu || { usagePercent: 88, memoryTotal: 23040, memoryUsed: 17408, temperature: 88 },
        system: { memoryTotal: 54181, memoryUsed: 3174, memoryUsagePercent: 5.8 },
        llamaCpp: { loadedModels: [{ name: 'Inkling-2-Mini-UD-Q4_K_M', contextLength: 32768 }], requestsPerSecond: 0 },
        ollama: {},
        lastSeen: new Date().toISOString(),
      }],
      backendMetrics: [],
      recentClients: [],
      totalRequests: 10,
      rps: 0,
      effectiveBackendType: 'llama_cpp',
      gpuMaxTemp: gpuMaxTemp === undefined ? 85 : gpuMaxTemp,
    },
    sessions: { sessions: [] },
    queueDetails: { pending_count: 0, processing_count: 0, all: [] },
    queueStats: { current_size: 0, processing_count: 0, admission: { enabled: true, waiting: 0 } },
    placement: { enabled: false, fallback: 'error', operatingMode: 'standard', decisions: [], warnings: [] },
    candidates: null,
    virtualModels: null,
    modelOps: null,
    autoPullConfig: null,
    autoPullStatus: null,
  };
}

function render(data) {
  const env = makeSandbox();
  loadRenderer(env.sandbox);
  env.sandbox.updateUI(data);
  return {
    html: env.sandbox.document.querySelector('#backendsTable tbody').innerHTML,
    alerts: String(env.sandbox.document.getElementById('alertContainer').innerHTML),
    calls: env.calls,
    errors: env.consoleErrors,
  };
}

console.log('monitor GPU temperature R757 (' + path.relative(REPO_ROOT, TARGET) + ')');

// --- 1. Температура выше порога: значение, красная подсветка, баннер ---------
{
  const r = render(sampleData({ usagePercent: 92, memoryTotal: 23040, memoryUsed: 17408, temperature: 88 }, 85));
  check('температура GPU нарисована в строке бэкенда (88 °C)',
    r.html.indexOf('88 °C') !== -1, r.html.slice(0, 400));
  check('подсветка «выше порога» — красная',
    r.html.indexOf('rgba(239,68,68') !== -1, r.html.slice(0, 400));
  check('в подсказке бейджа назван порог балансера (85 °C)',
    r.html.indexOf('monitor.gpu.tempThreshold') !== -1, r.html.slice(0, 400));
  check('баннер «GPU горячее порога» отрисован',
    r.alerts.indexOf('monitor.alert.gpuHot') !== -1, r.alerts.slice(0, 300));
  check('баннер называет узел, температуру и порог',
    r.alerts.indexOf('A10-126') !== -1 && r.alerts.indexOf('88') !== -1 && r.alerts.indexOf('85') !== -1,
    r.alerts.slice(0, 300));
  check('цепочка панелей дошла до конца', r.calls.topology === 1, 'вызовов: ' + r.calls.topology);
  check('ни одна панель не упала',
    r.errors.filter(function (m) { return m.indexOf('failed') !== -1; }).length === 0,
    r.errors.join(' | '));
}

// --- 2. В пределах 10 °C до порога: жёлтая подсветка ------------------------
{
  const r = render(sampleData({ usagePercent: 88, memoryTotal: 23040, memoryUsed: 17408, temperature: 78 }, 85));
  check('подсветка «близко к порогу» — жёлтая',
    r.html.indexOf('rgba(240,173,78') !== -1, r.html.slice(0, 400));
  check('при 78 °C и пороге 85 °C баннера «горячее порога» нет',
    r.alerts.indexOf('monitor.alert.gpuHot') === -1, r.alerts.slice(0, 300));
  check('температура при этом всё равно видна (78 °C)', r.html.indexOf('78 °C') !== -1);
}

// --- 3. Далеко от порога: нейтральная подсветка, без баннера ----------------
{
  const r = render(sampleData({ usagePercent: 40, memoryTotal: 23040, memoryUsed: 17408, temperature: 60 }, 85));
  check('при 60 °C и пороге 85 °C нейтральная подсветка (не красная)',
    r.html.indexOf('rgba(239,68,68') === -1 && r.html.indexOf('rgba(240,173,78') === -1,
    r.html.slice(0, 400));
  check('баннера о температуре нет',
    r.alerts.indexOf('monitor.alert.gpuHot') === -1 && r.alerts.indexOf('monitor.alert.gpuThrottle') === -1,
    r.alerts.slice(0, 300));
}

// --- 4. Thermal-троттлинг из NVML: виден даже ниже порога -------------------
// Смысл: 84 °C при пороге 85 - это НЕ «всё хорошо», если карта уже сообщает
// hw_thermal_slowdown. Предупреждение балансера (soft) и факт троттлинга
// железа - разные вещи, и панель обязана показать второе.
{
  const r = render(sampleData({
    usagePercent: 99,
    memoryTotal: 23040,
    memoryUsed: 17408,
    temperature: 84,
    throttleKnown: true,
    throttleReasons: 8,
    throttleReasonsList: ['hw_thermal_slowdown'],
  }, 85));
  check('факт троттлинга карты попал в строку (🔥)',
    r.html.indexOf('🔥') !== -1, r.html.slice(0, 400));
  check('причина троттлинга в подсказке бейджа',
    r.html.indexOf('hw_thermal_slowdown') !== -1, r.html.slice(0, 400));
  check('баннер о троттлинге (а не «мягкий порог») отрисован',
    r.alerts.indexOf('monitor.alert.gpuThrottle') !== -1, r.alerts.slice(0, 300));
  check('баннер о троттлинге называет причину от карты',
    r.alerts.indexOf('hw_thermal_slowdown') !== -1, r.alerts.slice(0, 300));
  check('красная подсветка даже ниже порога (троттлинг = снижение частот)',
    r.html.indexOf('rgba(239,68,68') !== -1, r.html.slice(0, 400));
}

// --- 5. Порог не задан (0): температура видна, оценки нет ------------------
{
  const r = render(sampleData({ usagePercent: 88, memoryTotal: 23040, memoryUsed: 17408, temperature: 88 }, 0));
  check('без порога температура всё равно нарисована', r.html.indexOf('88 °C') !== -1, r.html.slice(0, 400));
  check('без порога подсветка нейтральная (не пугает без настройки)',
    r.html.indexOf('rgba(239,68,68') === -1, r.html.slice(0, 400));
  check('без порога баннера о температуре нет',
    r.alerts.indexOf('monitor.alert.gpuHot') === -1, r.alerts.slice(0, 300));
}

// --- 6. Температура не сообщается: строку не ломаем ------------------------
{
  const r = render(sampleData({ usagePercent: 0, memoryTotal: 23040, memoryUsed: 0 }, 85));
  check('без температуры (°C = 0) лишнего бейджа нет',
    r.html.indexOf('0 °C') === -1, r.html.slice(0, 400));
  check('без температуры рендер не падает', r.calls.topology === 1);
}

console.log('');
if (failures > 0) {
  console.log('ИТОГ: провалено проверок — ' + failures);
  process.exit(1);
}
console.log('ИТОГ: все проверки пройдены');
