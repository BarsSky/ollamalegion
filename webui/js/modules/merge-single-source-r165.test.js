// merge-single-source-r165.test.js — R165 (2026-10-17): слияние дискового списка
// и загруженных моделей живёт в ОДНОМ месте.
//
// ЗАЧЕМ. Копий было две — в gguf-renderer-refresh.js и
// gguf-renderer-detail-render.js. Они совпадали только потому, что их правили
// вместе: R156 («модель поднята под алиасом — карточка её не видит») потребовал
// правки в обоих файлах, а вторая копия ещё и перекрывала первую при загрузке.
// Плюс экспорт M.mergeModelRows стоял ВНУТРИ refreshDetail(), поэтому до первого
// обновления панели функции не было вовсе — и вторая копия это маскировала.
//
// КОНТРФАКТУАЛЬНОСТЬ: на старом коде первый тест падал (две реализации), второй —
// тоже (M.mergeModelRows отсутствовал до вызова refreshDetail).
//
// Запуск: node webui/js/modules/merge-single-source-r165.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const results = [];
function check(name, fn) {
    try { fn(); results.push({ ok: true, name: name }); }
    catch (e) { results.push({ ok: false, name: name, err: e }); }
}

const refreshSrc = fs.readFileSync(path.join(__dirname, 'gguf-renderer-refresh.js'), 'utf8');
const detailSrc = fs.readFileSync(path.join(__dirname, 'gguf-renderer-detail-render.js'), 'utf8');

function countDefs(src) {
    return (src.match(/function\s+mergeModelRows\s*\(/g) || []).length;
}

// ── 1. Ровно одна реализация на весь модуль ─────────────────────────────────
check('реализация mergeModelRows ровно одна (refresh) и в detail-render её нет', function () {
    assert.strictEqual(countDefs(refreshSrc), 1,
        'в gguf-renderer-refresh.js должна быть ровно одна реализация, найдено: ' + countDefs(refreshSrc));
    assert.strictEqual(countDefs(detailSrc), 0,
        'в gguf-renderer-detail-render.js осталась вторая реализация — R156 снова придётся править дважды');
});

// ── 2. Экспорт доступен СРАЗУ при загрузке, а не после первого обновления ───
check('M.mergeModelRows доступен сразу при загрузке модулей', function () {
    global.window = global;
    global.Utils = { escapeHtml: function (s) { return String(s == null ? '' : s); } };
    global.document = {
        querySelector: function () { return null; },
        querySelectorAll: function () { return []; },
        getElementById: function () { return null; },
        createElement: function () { return {}; },
        addEventListener: function () {},
    };
    global.window.GgufModule = global.window.GgufModule || {};
    require('./gguf-renderer-refresh.js');
    require('./gguf-renderer-detail-render.js');
    const M = global.window.GgufModule;
    assert.strictEqual(typeof M.mergeModelRows, 'function',
        'M.mergeModelRows не функция до вызова refreshDetail — экспорт спрятан внутри функции');
});

// ── 3. Поведение не потеряно при сведении к одной реализации ────────────────
check('алиас по пути по-прежнему сливается с дисковым рядом', function () {
    const M = global.window.GgufModule;
    const rows = M.mergeModelRows(
        [{ name: 'gemma-4-E4B-it-Q4_K_M.gguf', path: '/app/models/gemma-4-E4B-it-Q4_K_M.gguf', state: 'available' }],
        [{ name: 'gemma-4', path: '/app/models/gemma-4-E4B-it-Q4_K_M.gguf' }]
    );
    assert.strictEqual(rows.length, 1, 'алиас создал второй ряд: ' + JSON.stringify(rows.map(function (r) { return r.name; })));
    assert.strictEqual(rows[0].state, 'loaded');
    assert.strictEqual(rows[0].loadedAs, 'gemma-4', 'имя, под которым реально загружено, потеряно');
});

let failed = 0;
console.log('');
results.forEach(function (r) {
    console.log((r.ok ? '  PASS  ' : '  FAIL  ') + r.name + (r.ok ? '' : '\n        ' + r.err.message));
    if (!r.ok) failed++;
});
console.log('\n' + (failed === 0 ? 'RESULT: PASS' : 'RESULT: FAIL (' + failed + ')'));
process.exit(failed === 0 ? 0 : 1);
