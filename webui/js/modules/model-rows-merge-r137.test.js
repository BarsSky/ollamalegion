// model-rows-merge-r137.test.js — R137 (2026-10-16).
//
// Запуск: node webui/js/modules/model-rows-merge-r137.test.js
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «во вкладке модели одна и та же модель появилась
// дважды, но с разными именами — Qwen3.8-27B-UD-Q4_K_M и
// Qwen3.8-27B-UD-Q4_K_M.gguf». Причина: списки (дисковый инвентарь и загруженные
// модели) сливались сравнением имён КАК ЕСТЬ; файл называется С расширением,
// инстанс — БЕЗ.
//
// Второй дефект того же класса: кнопка «Выгрузить» в списке диска уходила в API с
// именем ФАЙЛА, а реестр воркера знает инстанс без расширения → 404 «model not
// currently loaded».
//
// Проверяется:
//   1. одна модель на диске и в памяти даёт ОДНУ строку, помеченную загруженной;
//   2. у этой строки handle — имя ЗАГРУЖЕННОГО инстанса (не имя файла);
//   3. загруженная модель, которой нет на диске, добавляется (поведение R116);
//   4. две разные модели на диске + одна загруженная = две строки, помечена нужная;
//   5. loadedHandleFor находит handle по нормализованному имени.
'use strict';

const assert = require('assert');
const path = require('path');

global.window = global;
const DICT = {
    'gguf.mp_now': 'сейчас', 'gguf.mp_auto': 'авто', 'gguf.mp_on': 'включено',
    'gguf.mp_off': 'выключено', 'gguf.mp_unknown': '—', 'gguf.mp_not_in_profile': '— не задано —',
    'gguf.mp_inherit': 'наследуется', 'gguf.mp_list_placeholder': 'через запятую'
};
global.window.GgufModule = {
    _: function (key, fallback) {
        if (Object.prototype.hasOwnProperty.call(DICT, key)) return DICT[key];
        return fallback === undefined ? key : fallback;
    },
    state: { loadedModels: [] },
    stripGGUF: function (name) { return String(name || '').replace(/\.gguf$/i, ''); }
};
global.I18N = { t: function (k) { return DICT[k] || k; } };
global.Utils = { escapeHtml: function (s) { return String(s == null ? '' : s); } };
global.document = {
    querySelector: function () { return null; }, querySelectorAll: function () { return []; },
    getElementById: function () { return null; }, createElement: function () { return {}; },
    addEventListener: function () {}
};

require(path.join(__dirname, 'gguf-renderer-refresh.js'));
require(path.join(__dirname, 'gguf-renderer-detail-render.js'));
const M = global.window.GgufModule;
const stripGGUF = function (s) { return String(s || '').replace(/\.gguf$/i, ''); };

let checks = 0;
function check(name, fn) { fn(); checks++; console.log('  ✓ ' + name); }

const DISK = [
    { name: 'Qwen3.8-27B-UD-Q4_K_M.gguf', size: 16464440224, quantization: 'Q4_K_M', state: 'available' },
    { name: 'gemma-4-E4B-it-Q4_K_M.gguf', size: 4215695776, quantization: 'Q4_K_M', state: 'available' }
];

check('R137: модель на диске и в памяти — ОДНА строка, помеченная загруженной', function () {
    const rows = M.mergeModelRows(DISK, [{ name: 'Qwen3.8-27B-UD-Q4_K_M', path: '/app/models/Qwen3.8-27B-UD-Q4_K_M.gguf' }]);
    assert.strictEqual(rows.length, 2, 'строк должно быть две (по числу файлов), а не три: ' + JSON.stringify(rows.map(r => r.name)));
    const q = rows.filter(r => stripGGUF(r.name) === 'Qwen3.8-27B-UD-Q4_K_M');
    assert.strictEqual(q.length, 1, 'одна и та же модель попала в список дважды (исходная жалоба)');
    assert.strictEqual(q[0].state, 'loaded', 'строка не помечена загруженной — кнопка предложит «Загрузить» уже загруженной модели');
    assert.strictEqual(q[0].name, 'Qwen3.8-27B-UD-Q4_K_M.gguf', 'имя файла потеряно (карточка диска должна остаться)');
});

check('R137: handle строки — имя ЗАГРУЖЕННОГО инстанса, а не имя файла', function () {
    const rows = M.mergeModelRows(DISK, [{ name: 'Qwen3.8-27B-UD-Q4_K_M', path: '/app/models/Qwen3.8-27B-UD-Q4_K_M.gguf' }]);
    const q = rows.find(r => stripGGUF(r.name) === 'Qwen3.8-27B-UD-Q4_K_M');
    assert.strictEqual(q.loadedHandle, 'Qwen3.8-27B-UD-Q4_K_M',
        'выгрузка ушла бы с именем файла и получила 404 «model not currently loaded»');
});

check('R137: загруженная модель без файла на диске добавляется (R116 не сломан)', function () {
    const rows = M.mergeModelRows([], [{ name: 'model-from-memory' }]);
    assert.strictEqual(rows.length, 1);
    assert.strictEqual(rows[0].name, 'model-from-memory');
    assert.strictEqual(rows[0].state, 'loaded');
});

check('R137: разные модели не склеиваются', function () {
    const rows = M.mergeModelRows(DISK, [{ name: 'gemma-4-E4B-it-Q4_K_M' }]);
    assert.strictEqual(rows.length, 2);
    assert.strictEqual(rows.find(r => stripGGUF(r.name) === 'gemma-4-E4B-it-Q4_K_M').state, 'loaded');
    assert.strictEqual(rows.find(r => stripGGUF(r.name) === 'Qwen3.8-27B-UD-Q4_K_M').state, 'available');
});

check('R137: loadedHandleFor находит handle по нормализованному имени', function () {
    M.state.loadedModels = [{ name: 'Qwen3.8-27B-UD-Q4_K_M', handle: 'Qwen3.8-27B-UD-Q4_K_M' }];
    assert.strictEqual(M.loadedHandleFor('Qwen3.8-27B-UD-Q4_K_M.gguf'), 'Qwen3.8-27B-UD-Q4_K_M');
    M.state.loadedModels = [];
    assert.strictEqual(M.loadedHandleFor('Qwen3.8-27B-UD-Q4_K_M.gguf'), 'Qwen3.8-27B-UD-Q4_K_M.gguf',
        'если модель не загружена, handle остаётся именем файла (для загрузки)');
});

check('R156: инстанс под АЛИАСОМ находится по пути файла', function () {
    // ЖИВОЙ ДЕФЕКТ: модель поднимали коротким именем `gemma-4`, файл — тот же
    // gemma-4-E4B-it-Q4_K_M.gguf. По имени строка не находилась, карточка показывала
    // «не загружена», фактов загрузки не было вовсе (чипов 0).
    const disk = [{ name: 'gemma-4-E4B-it-Q4_K_M', path: '/app/models/gemma-4-E4B-it-Q4_K_M.gguf', state: 'available' }];
    const loaded = [{ name: 'gemma-4', path: '/app/models/gemma-4-E4B-it-Q4_K_M.gguf', handle: 'gemma-4' }];
    const rows = M.mergeModelRows(disk, loaded);
    assert.strictEqual(rows.length, 1, 'алиас не создаёт вторую строку: ' + JSON.stringify(rows.map(r => r.name)));
    assert.strictEqual(rows[0].state, 'loaded', 'дисковая строка помечена загруженной');
    assert.strictEqual(rows[0].loadedHandle, 'gemma-4', 'выгрузка пойдёт по имени инстанса');
    assert.strictEqual(rows[0].loadedAs, 'gemma-4', 'видно, ПОД КАКИМ именем загружено');
});

check('R156: без пути работает прежнее сопоставление по имени', function () {
    const disk = [{ name: 'M', state: 'available' }];
    const rows = M.mergeModelRows(disk, [{ name: 'M' }]);
    assert.strictEqual(rows.length, 1);
    assert.strictEqual(rows[0].state, 'loaded');
    assert.strictEqual(rows[0].loadedAs, undefined, 'совпадение по имени — алиаса нет');
});

check('R156: чужой путь не приклеивается к строке', function () {
    const disk = [{ name: 'M', path: '/m/M.gguf', state: 'available' }];
    const rows = M.mergeModelRows(disk, [{ name: 'other', path: '/m/other.gguf' }]);
    assert.strictEqual(rows.length, 2, 'разные файлы — две строки');
    assert.strictEqual(rows.find(r => r.name === 'M').state, 'available');
    assert.strictEqual(rows.find(r => r.name === 'other').state, 'loaded');
});

console.log('\nmodel-rows-merge-r137: ' + checks + ' проверок пройдено');
