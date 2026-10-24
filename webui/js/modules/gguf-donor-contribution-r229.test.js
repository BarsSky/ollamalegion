// gguf-donor-contribution-r229.test.js — R229 (2026-10-22).
//
// Запуск: node webui/js/modules/gguf-donor-contribution-r229.test.js
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «видно, что модель разложена на донора, но не видно, ЧТО
// именно он даёт» — какая это модель, сколько слоёв на узле и сколько памяти.
//
// Карточка строится по ТОЧНОЙ раскладке воркера (device_layers_exact — перехваченные
// строки `load_tensors: layer N assigned to device RPC0`); у RPC-диапазонов есть
// node. Память — ОЦЕНКА по размеру модели и доле слоёв, и это подписано словами:
// фактическую память узла воркер по-узлово не отдаёт, выдавать догадку за факт
// нельзя.
//
// Проверяется:
//   1. RPC-диапазон с node попадает в карточку: узел, «N из L», оценка МБ;
//   2. локальные CUDA/CPU в карточку не попадают (это не вклад донора);
//   3. без RPC-слоёв карточки нет вовсе;
//   4. без размера модели память не выдумывается («—»);
//   5. постоянная ошибка прошлого: донор с 0 слоёв не показывается.

'use strict';

const assert = require('assert');

global.window = global;
global.MonitorApp = { esc: function (s) { return String(s == null ? '' : s); } };
global.Utils = {
    escapeHtml: function (s) {
        return String(s == null ? '' : s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    }
};
global.I18N = { t: function (key, fallback) { return fallback === undefined ? key : fallback; } };
global.window.I18N = global.I18N;
global.document = {
    querySelector: function () { return null; },
    querySelectorAll: function () { return []; },
    getElementById: function () { return null; },
    createElement: function () { return {}; },
    addEventListener: function () {}
};
global.window.GgufModule = {
    _: function (key, fallback) { return fallback === undefined ? key : fallback; }
};
const fs = require('fs');
const path = require('path');
const SRC = path.join(__dirname, 'gguf-renderer-detail-render.js');
require(SRC);
const M = global.window.GgufModule;

let checks = 0;
function check(name, fn) { fn(); checks++; console.log('  ✓ ' + name); }

// Живой факт стенда (раскладка gemma-4-E4B-it-Q4_K_M с донором).
const LOADED = {
    name: 'gemma-4-E4B-it-Q4_K_M',
    n_layers: 42,
    size_bytes: 4215695776,
    device_layers_exact: [
        { device: 'CPU', from: 0, to: 0, count: 1 },
        { device: 'CUDA0', from: 1, to: 23, count: 23 },
        { device: 'RPC0', from: 24, to: 42, count: 19, node: '192.0.2.10:50052' }
    ]
};

check('R229: карточка вклада донора показывает узел, слои и оценку памяти', function () {
    const html = M.donorContributionHtml(LOADED);
    assert.ok(/gguf-donor-card/.test(html), 'карточки нет: ' + html);
    assert.ok(/Вклад доноров/.test(html), 'нет заголовка карточки');
    assert.ok(/192\.0\.2\.10:50052/.test(html), 'в карточке нет адреса узла');
    assert.ok(/19/.test(html) && /из 42/.test(html), 'нет «19 из 42»: ' + html);
    assert.ok(/~1819 МБ/.test(html), 'нет оценки памяти по доле слоёв: ' + html);
    assert.ok(/оценка/i.test(html), 'оценка не подписана словами');
});

check('R229: локальные CUDA/CPU не выдаются за вклад донора', function () {
    const html = M.donorContributionHtml(LOADED);
    assert.ok(!/CUDA0/.test(html), 'локальная карта попала в карточку доноров: ' + html);
    assert.ok(!/>CPU</.test(html), 'CPU попал в карточку доноров: ' + html);
});

check('R229: без RPC-слоёв карточки нет', function () {
    const localOnly = {
        name: 'm', n_layers: 42, size_bytes: 100,
        device_layers_exact: [{ device: 'CPU', count: 1 }, { device: 'CUDA0', count: 41 }]
    };
    assert.strictEqual(M.donorContributionHtml(localOnly), '', 'карточка появилась без донора');
    assert.strictEqual(M.donorContributionHtml({ name: 'm' }), '', 'карточка появилась без раскладки');
});

check('R229: без размера модели память не выдумывается', function () {
    const noSize = {
        name: 'm', n_layers: 42,
        device_layers_exact: [{ device: 'RPC0', count: 10, node: 'donor:50052' }]
    };
    const html = M.donorContributionHtml(noSize);
    assert.ok(/donor:50052/.test(html), 'нет узла');
    assert.ok(!/МБ/.test(html), 'память нарисована без размера модели: ' + html);
});

check('R229: узел с нулём слоёв не показывается', function () {
    const zero = {
        name: 'm', n_layers: 42, size_bytes: 1000,
        device_layers_exact: [{ device: 'RPC0', count: 0, node: 'donor:50052' }]
    };
    assert.strictEqual(M.donorContributionHtml(zero), '', 'узел с 0 слоёв попал в карточку');
});

console.log('\nOK: ' + checks + ' checks passed');
