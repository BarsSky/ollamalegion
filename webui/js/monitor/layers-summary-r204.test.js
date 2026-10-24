// layers-summary-r204.test.js — R204 (2026-10-21).
// Форматирование распределения слоёв раскладки: «локально N / на узлах M» с адресами
// доноров. Живой случай, ради которого это появилось: оператор не видел НИГДЕ, сколько
// слоёв ушло на доноров.
'use strict';
const assert = require('assert');
global.window = global;
global.MonitorApp = { T: function (k, f) { return f; } };
let checks = 0;
function check(name, fn) { fn(); checks++; console.log('  ✓ ' + name); }

// подгружаем только чистую функцию: файл — IIFE с DOM-зависимостями, поэтому
// извлекаем layersSummaryText через Function-конструктор (как делает браузер).
const fs = require('fs');
const path = require('path');
const src = fs.readFileSync(path.join(__dirname, 'layout-panel.js'), 'utf8');
const m = src.match(/function layersSummaryText\(target, t\) \{[\s\S]*?\n  \}/);
assert.ok(m, 'layersSummaryText не найдена в layout-panel.js');
const layersSummaryText = new Function('return ' + m[0].replace('function layersSummaryText', 'function'))();
const T = function (k, f) { return f; };

check('RPC-слои и адрес донора попадают в подпись', function () {
  const rows = [
    { device: 'CPU', from: 0, to: 0, count: 1 },
    { device: 'CUDA0', from: 1, to: 23, count: 23 },
    { device: 'RPC0', from: 24, to: 42, count: 19, node: '192.0.2.10:50052' }
  ];
  const text = layersSummaryText(rows, T);
  assert.ok(/CUDA0 23/.test(text), 'нет локальных слоёв: ' + text);
  assert.ok(/RPC0 19 \(192\.0\.2\.10:50052\)/.test(text), 'нет донора с адресом: ' + text);
  assert.ok(/локально 23/.test(text) && /на узлах 19/.test(text), 'нет сводки: ' + text);
});

check('несколько доноров суммируются в «на узлах»', function () {
  const rows = [
    { device: 'CUDA0', count: 10 },
    { device: 'RPC0', count: 5, node: 'a:50052' },
    { device: 'RPC1', count: 6, node: 'b:50052' }
  ];
  const text = layersSummaryText(rows, T);
  assert.ok(/на узлах 11/.test(text), 'сумма доноров неверна: ' + text);
  assert.ok(/RPC1 6 \(b:50052\)/.test(text), 'второй донор не показан: ' + text);
});

check('без раскладки (только локальные) — «на узлах 0»', function () {
  const rows = [{ device: 'CUDA0', count: 43 }];
  const text = layersSummaryText(rows, T);
  assert.ok(/локально 43/.test(text) && /на узлах 0/.test(text), text);
});

console.log('\nOK: ' + checks + ' проверок пройдено');
