// window-align-r494.test.js — R494 (2026-11-12, запрос оператора).
//
// ЖИВОЙ ФАКТ (стенд A10 ×2, Qwen3.8-27B): на мастере копия с окном 198656, на
// A10-126 — с 131072. Монитор показывал «два бэкенда», но не говорил, что узел
// простаивает из-за разного окна; оператор просил уведомление с возможностью
// подтвердить увеличение окна.
//
// ПРАВИЛО ОПЕРАТОРА: «если копия не занята сессией — решение по выравниванию;
// если занята, а есть бэкенд с нужным окном — ставить в очередь к нему».
// Поэтому баннер показывается ТОЛЬКО для свободной узкой копии: у занятой
// «выровнять» означало бы оборвать чужую генерацию.
//
// ЧИСЛА — из балансера (GET /api/v1/window/align): в браузере их считать нельзя,
// там нет ни сессий с активными запросами, ни потолка LB_WINDOW_ALIGN_MAX_N_CTX.
// Здесь проверяется ПРАВИЛО ПОКАЗА (state.js::windowAlignAlerts), а не арифметика.
//
// Запуск: node webui/js/monitor/window-align-r494.test.js
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

// --- загрузка state.js в минимальном окружении (как в R493-тесте) -----------
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
assert.strictEqual(typeof MA.windowAlignAlerts, 'function', 'windowAlignAlerts отсутствует в state.js');

// T-заглушка: подставляет параметры в шаблон, чтобы проверить и числа в тексте.
function T(key, params) {
  params = params || {};
  return key + '|' + (params.model || '') + '|' + (params.backend || '') + '|' +
    (params.current === undefined ? '' : params.current) + '|' +
    (params.target === undefined ? '' : params.target);
}

const MODEL = 'Qwen3.8-27B-UD-Q4_K_M';

// Ответ балансера по архиву: мастер (занят, 198656) + A10-126 (свободен, 131072).
const archiveSuggestion = {
  model: MODEL,
  cap: 262144,
  targetWindow: 198656,
  copies: [
    { backendId: 'A10-126', window: 131072, sessions: 0, active: 0, free: true },
    { backendId: 'cppworker-gpu-bundled-agent', window: 198656, sessions: 1, active: 1, free: false }
  ],
  alignableBackends: ['A10-126'],
  busyBackends: []
};

console.log('R494: выравнивание окна копии — баннер и цель');

check('баннер есть: свободная узкая копия с числом и целью', function () {
  const out = MA.windowAlignAlerts([archiveSuggestion], T);
  assert.strictEqual(out.length, 1, 'баннер не построен для свободной узкой копии');
  const a = out[0];
  assert.strictEqual(a.level, 'yellow');
  assert.strictEqual(a.action, 'windowAlign');
  assert.strictEqual(a.model, MODEL);
  assert.strictEqual(a.backendId, 'A10-126');
  assert.strictEqual(a.target, 198656, 'цель должна быть окном самой широкой копии');
  assert.ok(/198656$/.test(a.text), 'в тексте нет целевого окна: ' + a.text);
  assert.ok(/131072/.test(a.text), 'в тексте нет текущего окна копии: ' + a.text);
});

check('занятая узкая копия не предлагается (alignableBackends пуст)', function () {
  const busy = {
    model: MODEL, cap: 262144, targetWindow: 198656,
    copies: [
      { backendId: 'A10-126', window: 131072, sessions: 1, active: 1, free: false },
      { backendId: 'cppworker-gpu-bundled-agent', window: 198656, sessions: 0, active: 0, free: true }
    ],
    alignableBackends: [],
    busyBackends: ['A10-126']
  };
  assert.strictEqual(MA.windowAlignAlerts([busy], T).length, 0,
    'баннер построен для занятой копии: перезагрузка оборвала бы чужую сессию');
});

check('нет предложений (пусто или null) — нет баннера', function () {
  assert.strictEqual(MA.windowAlignAlerts([], T).length, 0);
  assert.strictEqual(MA.windowAlignAlerts(null, T).length, 0);
  assert.strictEqual(MA.windowAlignAlerts(undefined, T).length, 0);
});

check('цель 0 — баннер не строится (окно неизвестно, предлагать нечего)', function () {
  const bad = {
    model: MODEL, cap: 0, targetWindow: 0,
    copies: [{ backendId: 'A10-126', window: 131072, sessions: 0, active: 0, free: true }],
    alignableBackends: ['A10-126']
  };
  assert.strictEqual(MA.windowAlignAlerts([bad], T).length, 0);
});

check('несколько свободных узких копий — вторая отражена в more', function () {
  const two = {
    model: MODEL, cap: 262144, targetWindow: 198656,
    copies: [
      { backendId: 'A10-126', window: 131072, sessions: 0, active: 0, free: true },
      { backendId: 'A10-127', window: 65536, sessions: 0, active: 0, free: true },
      { backendId: 'master', window: 198656, sessions: 0, active: 0, free: true }
    ],
    alignableBackends: ['A10-126', 'A10-127']
  };
  const out = MA.windowAlignAlerts([two], T);
  assert.strictEqual(out.length, 1);
  assert.strictEqual(out[0].backendId, 'A10-126');
  assert.strictEqual(out[0].more, 1, 'должно быть видно, что есть ещё одна свободная копия');
});

check('две модели в предложении — два баннера', function () {
  const other = Object.assign({}, archiveSuggestion, { model: 'gemma-4-E4B' });
  const out = MA.windowAlignAlerts([archiveSuggestion, other], T);
  assert.strictEqual(out.length, 2);
  const models = out.map(function (a) { return a.model; }).sort();
  assert.deepStrictEqual(models, [MODEL, 'gemma-4-E4B'].sort());
});

console.log('');
if (failures.length) {
  console.error('ИТОГ: FAIL (' + failures.length + ')');
  failures.forEach(function (f) { console.error(' - ' + f); });
  process.exit(1);
}
console.log('ИТОГ: PASS (' + passed + ' проверок)');
