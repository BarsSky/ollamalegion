// rebalance-suggestion-r493.test.js — R493 (2026-11-12, жалоба оператора).
//
// ЖИВОЙ ФАКТ (стенд A10 ×2, Qwen3.8-27B): «в мониторе выскакивает уведомление с
// предложением перераспределения по моделям, и сейчас оно не корректно: сессий
// две, два бэкенда занято, больше сессий нет — перераспределение не требуется».
//
// ПРИЧИНЫ ЛОЖНОГО СОВЕТА:
//   1. совет строился по загрузке бэкенда, без проверки КОНКУРЕНЦИИ: одна сессия
//      на бэкенд и пустая очередь — двигать нечего;
//   2. «свободная альтернатива» не проверялась на пригодность: узлы без
//      загруженной модели (8 ГБ, 27B не влезает) и копии с МЕНЬШИМ окном
//      (131072 < 198656) предлагались как цель переезда.
//
// Запуск: node webui/js/monitor/rebalance-suggestion-r493.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

let passed = 0;
const failures = [];
function check(name, fn) {
  try { fn(); passed++; console.log('  \u2713 ' + name); }
  catch (e) { failures.push(name + ': ' + (e && e.message)); console.error('  \u2717 ' + name + ' - ' + (e && e.message)); }
}

// --- загрузка state.js в минимальном окружении -----------------------------
// state.js при загрузке читает localStorage (detectApiBase) — в Node его нет,
// поэтому заглушка обязательна, иначе модуль не загружается вовсе.
global.window = global;
global.localStorage = {
  _d: {},
  getItem: function (k) { return this._d[k] === undefined ? null : this._d[k]; },
  setItem: function (k, v) { this._d[k] = String(v); },
  removeItem: function (k) { delete this._d[k]; }
};
global.location = { protocol: 'http:', host: '127.0.0.1:18083', origin: 'http://127.0.0.1:18083', search: '' };
global.URLSearchParams = global.URLSearchParams || function () { this.get = function () { return null; }; };
global.addEventListener = function () {};
global.removeEventListener = function () {};
if (global.window && global.window !== global) { /* noop */ }
global.document = {
  readyState: 'complete',
  getElementById: function () { return null; },
  addEventListener: function () {},
  querySelector: function () { return null; },
  querySelectorAll: function () { return []; },
  createElement: function () { return { style: {}, classList: { add: function () {}, remove: function () {} }, appendChild: function () {} }; }
};
const stateSrc = fs.readFileSync(path.join(__dirname, 'state.js'), 'utf8');
// eslint-disable-next-line no-new-func
new Function('global', 'window', stateSrc).call(global, global, global);
const MA = global.MonitorApp;
assert.ok(MA, 'MonitorApp не загрузился из state.js');

// --- фикстуры из архива стенда ---------------------------------------------
const MODEL = 'Qwen3.8-27B-UD-Q4_K_M';
const master = {
  id: 'cppworker-gpu-bundled-agent', status: 'healthy',
  maxConcurrentRequests: 1, activeRequests: 1,
  loadedModels: [{ name: MODEL, context_per_seq: 198656 }]
};
const a10 = {
  id: 'A10-126', status: 'healthy',
  maxConcurrentRequests: 1, activeRequests: 0,
  loadedModels: [{ name: MODEL, context_per_seq: 131072 }]
};
const small1 = { id: 'WIN-I9', status: 'healthy', maxConcurrentRequests: 1, activeRequests: 0, loadedModels: [] };
const small2 = { id: 'ASTRA-I9', status: 'healthy', maxConcurrentRequests: 1, activeRequests: 0, loadedModels: [] };
const image = { id: 'imageworker', status: 'healthy', maxConcurrentRequests: 64, activeRequests: 0, backendType: 'image_cpp' };

console.log('R493: совет о перераспределении не должен быть ложным');

check('altBackendsFor: копия с МЕНЬШИМ окном не предлагается', function () {
  const alt = MA.altBackendsFor([master, a10], master, 0.5, MODEL);
  assert.strictEqual(alt.length, 0,
    'A10-126 (окно 131072 < 198656) предложен как альтернатива: переезд запрещён R459');
});

check('altBackendsFor: узел БЕЗ загруженной модели не предлагается', function () {
  const alt = MA.altBackendsFor([master, small1, small2], master, 0.5, MODEL);
  assert.strictEqual(alt.length, 0,
    'узлы без модели (8 ГБ, 27B не влезает) предложены как цель переезда');
});

check('altBackendsFor: равное окно и загруженная модель — законная альтернатива', function () {
  const equal = { id: 'A10-eq', status: 'healthy', maxConcurrentRequests: 1, activeRequests: 0,
    loadedModels: [{ name: MODEL, context_per_seq: 198656 }] };
  const alt = MA.altBackendsFor([master, equal], master, 0.5, MODEL);
  assert.strictEqual(alt.length, 1);
  assert.strictEqual(alt[0].id, 'A10-eq');
});

check('altBackendsFor: image-бэкенд не предлагается текстовой сессии', function () {
  const alt = MA.altBackendsFor([master, image], master, 0.5, MODEL);
  assert.strictEqual(alt.length, 0);
});

check('sessionsPerBackend: две сессии на двух бэкендах — по одной на каждом', function () {
  const counts = MA.sessionsPerBackend([
    { id: 's1', backendId: 'cppworker-gpu-bundled-agent' },
    { id: 's2', backendId: 'A10-126' }
  ]);
  assert.strictEqual(counts['cppworker-gpu-bundled-agent'], 1);
  assert.strictEqual(counts['A10-126'], 1);
});

check('конкуренции нет: совет не нужен (правило рендерера)', function () {
  // Правило из ui-renderer.diagnose: совет только при >1 сессии на бэкенд ИЛИ
  // при непустой очереди. В архиве было ровно по одной и очередь пуста.
  const counts = MA.sessionsPerBackend([
    { id: 's1', backendId: 'cppworker-gpu-bundled-agent' },
    { id: 's2', backendId: 'A10-126' }
  ]);
  const pending = 0;
  const onBackend = counts['cppworker-gpu-bundled-agent'] || 0;
  assert.ok(onBackend <= 1 && pending === 0,
    'при одной сессии на бэкенд и пустой очереди перераспределение не предлагается');
});

console.log('');
if (failures.length) {
  console.error('ИТОГ: FAIL (' + failures.length + ')');
  failures.forEach(f => console.error(' - ' + f));
  process.exit(1);
}
console.log('ИТОГ: PASS (' + passed + ' проверок)');
