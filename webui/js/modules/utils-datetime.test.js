// utils-datetime.test.js — R-Image follow-up (2026-10-02): форматирование
// timestamp'ов в WebUI.
//
// Запуск: node webui/js/modules/utils-datetime.test.js
//
// ЗАЧЕМ. Живой стенд показал дефект: у image-бэкенда (у него нет агента и
// heartbeat) в колонке «Последняя активность» стояло «01.01.1, 02:30:17».
// Причина — Go отдаёт НУЛЕВОЕ время строкой "0001-01-01T00:00:00Z", а строка
// истинна, поэтому проверка на falsy её пропускала и `new Date(...)` исправно
// рисовал дату 0001 года. Тесты фиксируют:
//   1. formatDate: пусто/нулевое время/мусор -> '-', реальная дата -> строка с годом.
//   2. formatTime: то же самое, но прочерк тайма '-—:--:--' вида '--:--:--'.
//   3. formatDateFirst: выбирается ПЕРВЫЙ ВАЛИДНЫЙ кандидат, а не первый truthy —
//      иначе нулевой heartbeat «заслонял» собой реальный lastAgentContact.

'use strict';

const assert = require('assert');

// --- минимальный DOM: utils.js трогает document/window при загрузке ---
function makeFakeElement() {
    let text = '';
    const el = { id: '', style: {}, dataset: {}, children: [] };
    Object.defineProperty(el, 'textContent', { get() { return text; }, set(v) { text = String(v); } });
    Object.defineProperty(el, 'innerHTML', {
        get() {
            return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
        },
    });
    return el;
}

const byId = {};
global.document = {
    createElement() { return makeFakeElement(); },
    getElementById(id) { return byId[id] || (byId[id] = makeFakeElement()); },
};
global.window = global;

require('./utils.js');
const U = global.window.Utils;
assert.ok(U, 'window.Utils должен быть экспортирован (см. check_iife_exports.py)');
assert.ok(typeof U.formatDate === 'function', 'Utils.formatDate должен быть экспортирован');
assert.ok(typeof U.formatTime === 'function', 'Utils.formatTime должен быть экспортирован');
assert.ok(typeof U.formatDateFirst === 'function', 'Utils.formatDateFirst должен быть экспортирован');

let passed = 0;
let failures = 0;
function check(name, fn) {
    try {
        fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures++;
        console.log('  \u2717 ' + name + ' — ' + (e && e.message));
    }
}

// Ровно то, что отдаёт живой балансер для image-бэкенда без агента.
const ZERO_TIME = '0001-01-01T00:00:00Z';
const REAL_TIME = '2026-10-02T19:34:05Z';

console.log('utils-datetime (' + require('path').basename(__filename) + ')');

check('formatDate: нулевое время Go -> "-" (а не 01.01.1)', function () {
    assert.strictEqual(U.formatDate(ZERO_TIME), '-');
});

check('formatDate: пустые значения -> "-"', function () {
    assert.strictEqual(U.formatDate(null), '-');
    assert.strictEqual(U.formatDate(undefined), '-');
    assert.strictEqual(U.formatDate(''), '-');
});

check('formatDate: мусор -> "-"', function () {
    assert.strictEqual(U.formatDate('not-a-date'), '-');
});

check('formatDate: реальная дата -> строка с годом 2026', function () {
    const s = U.formatDate(REAL_TIME);
    assert.notStrictEqual(s, '-');
    assert.ok(s.indexOf('2026') !== -1, 'ожидали год 2026, получили: ' + s);
});

check('formatDate: дата 1970-01-01 (эпоха) остаётся валидной', function () {
    assert.notStrictEqual(U.formatDate('1970-01-01T00:00:00Z'), '-');
});

check('formatTime: нулевое время Go -> "--:--:--"', function () {
    assert.strictEqual(U.formatTime(ZERO_TIME), '--:--:--');
});

check('formatTime: реальное время -> не прочерк', function () {
    assert.notStrictEqual(U.formatTime(REAL_TIME), '--:--:--');
});

check('formatDateFirst: нулевой heartbeat не заслоняет реальный contact', function () {
    assert.strictEqual(U.formatDateFirst(ZERO_TIME, REAL_TIME), U.formatDate(REAL_TIME));
});

check('formatDateFirst: реальный heartbeat выигрывает у contact', function () {
    assert.strictEqual(U.formatDateFirst(REAL_TIME, ZERO_TIME), U.formatDate(REAL_TIME));
});

check('formatDateFirst: все кандидаты невалидны -> "-"', function () {
    assert.strictEqual(U.formatDateFirst(ZERO_TIME, null, ''), '-');
});

console.log('');
if (failures > 0) {
    console.log('ИТОГ: провалено проверок — ' + failures);
    process.exit(1);
}
console.log('ИТОГ: все проверки пройдены (' + passed + ')');
