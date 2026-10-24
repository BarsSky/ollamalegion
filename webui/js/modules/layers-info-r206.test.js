// layers-info-r206.test.js — R206 (2026-10-21).
// Строка распределения слоёв в «Бэкендах»: чистое форматирование и скрытие без раскладки.
'use strict';
const assert = require('assert');
const fs = require('fs');
const path = require('path');
const src = fs.readFileSync(path.join(__dirname, 'layout-layers-info.js'), 'utf8');
const m = src.match(/function text\(deviceLayers, translator\) \{[\s\S]*?\n  \}/);
assert.ok(m, 'text() не найдена в layout-layers-info.js');
const text = new Function('return ' + m[0].replace('function text', 'function'))();
const T = function (k, f) { return f; };
let checks = 0;
function check(name, fn) { fn(); checks++; console.log('  ✓ ' + name); }

check('показывает локальные слои и донора с адресом', function () {
  const s = text([
    { device: 'CPU', count: 1 },
    { device: 'CUDA0', count: 23 },
    { device: 'RPC0', count: 19, node: '192.0.2.10:50052' }
  ], T);
  assert.ok(/CUDA0 23/.test(s), s);
  assert.ok(/RPC0 19 \(192\.0\.2\.10:50052\)/.test(s), s);
  assert.ok(/локально 23/.test(s) && /на узлах 19/.test(s), s);
});

check('несколько доноров суммируются', function () {
  const s = text([{ device: 'CUDA0', count: 10 }, { device: 'RPC0', count: 5, node: 'a:50052' }, { device: 'RPC1', count: 6, node: 'b:50052' }], T);
  assert.ok(/на узлах 11/.test(s), s);
});

check('пустые данные → пустая строка (блок скрыт)', function () {
  assert.strictEqual(text([], T), '');
  assert.strictEqual(text(null, T), '');
});

check('без раскладки показано «на узлах 0»', function () {
  const s = text([{ device: 'CUDA0', count: 43 }], T);
  assert.ok(/на узлах 0/.test(s), s);
});

console.log('\nOK: ' + checks + ' проверок пройдено');
