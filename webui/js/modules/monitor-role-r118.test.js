// monitor-role-r118.test.js — R118 (2026-10-14).
//
// Запуск: node webui/js/modules/monitor-role-r118.test.js
//
// ТРЕБОВАНИЕ ОПЕРАТОРА: «явное управление от балансера через WEBUI, чтобы можно
// было в панели настроить режим для бэкенда, указав кто донор, а кто главный или
// же самодостаточный».
//
// Что фиксирует тест:
//   1. у llama.cpp-бэкенда в строке есть селектор роли с четырьмя значениями
//      (не задана / самодостаточный / главный / донор), и выбран ТЕКУЩИЙ;
//   2. роли приходят с балансера (поле role), пустая роль = «не задана»;
//   3. у image-воркера селектора нет (раскладка слоёв к нему не относится);
//   4. смена значения в селекторе уходит в Api.setBackendRole(bid, role).

'use strict';

const assert = require('assert');

global.window = global;
global.MonitorApp = {
    esc: function (s) { return String(s === null || s === undefined ? '' : s); },
    fmtDur: function () { return '-'; }
};
global.I18N = {
    t: function (key) {
        const map = {
            'monitor.role.none': '— не задана —',
            'monitor.role.standalone': 'самодостаточный',
            'monitor.role.main': 'главный (раскладка)',
            'monitor.role.donor': 'донор (узел)',
            'monitor.role.title': 'Роль бэкенда в раскладке слоёв',
            'monitor.common.noData': 'Нет данных'
        };
        return Object.prototype.hasOwnProperty.call(map, key) ? map[key] : key;
    }
};

// DOM-заглушка: renderTable пишет в #backendsTable tbody и навешивает селекторы.
const listeners = [];
function makeEl(id) {
    return {
        id: id, innerHTML: '', textContent: '', value: '', style: {}, dataset: {},
        classList: { add() {}, remove() {}, contains() { return false; } },
        addEventListener: function (ev, fn) { listeners.push({ el: this, ev: ev, fn: fn }); },
        querySelectorAll: function () { return []; },
        setAttribute() {}, getAttribute() { return null; }, appendChild() {}
    };
}
const els = { backendsTable: makeEl('backendsTable') };
// tbody таблицы бэкендов: renderTable берёт его через document.querySelector.
const stubTbody = {
    innerHTML: '',
    querySelectorAll: function () { return []; }
};
els.backendsTable.querySelector = function (q) { return q === 'tbody' ? stubTbody : null; };
global.document = {
    querySelector: function (sel) {
        if (sel === '#backendsTable tbody') return stubTbody;
        return els[sel] || null;
    },
    getElementById: function (id) { return els[id] || null; },
    querySelectorAll: function () { return []; },
    createElement: function () { return makeEl(''); },
    addEventListener: function () {}
};

global.Utils = { showToast: function () {}, escapeHtml: function (s) { return String(s); } };

// Модуль объявляет `const MonitorBackends = (() => {...})()` и в браузере живёт
// глобальной переменной; в CommonJS его надо скомпилировать с экспортом.
const fs = require('fs');
const path = require('path');
const NodeModule = require('module');
const SRC = path.join(__dirname, 'monitor-backends.js');
const m = new NodeModule(SRC, module);
m._compile(fs.readFileSync(SRC, 'utf8') + '\nmodule.exports = MonitorBackends;', SRC);
const MB = m.exports;
global.window.MonitorBackends = MB;

let checks = 0;
function check(name, fn) { fn(); checks++; console.log('  ✓ ' + name); }

const LLAMA = { id: 'cppworker-gpu-bundled-agent', type: 'llama_cpp', role: 'main', status: 'healthy' };
const DONOR = { id: 'cppworker-cyborg-bundled-agent', type: 'llama_cpp', role: 'donor', status: 'healthy' };
const IMAGE = { id: 'imageworker', type: 'image_cpp', status: 'healthy' };

check('R118: у llama.cpp-бэкенда есть селектор роли с четырьмя значениями', function () {
    const html = MB.roleSelect(LLAMA);
    assert.ok(/backend-role-select/.test(html), 'нет селектора роли: ' + html);
    assert.ok(/data-backend="cppworker-gpu-bundled-agent"/.test(html), 'нет id бэкенда');
    ['', 'standalone', 'main', 'donor'].forEach(function (v) {
        assert.ok(new RegExp('<option value="' + v + '"').test(html), 'нет варианта роли "' + v + '"');
    });
    assert.ok(/самодостаточный/.test(html) && /главный \(раскладка\)/.test(html) && /донор \(узел\)/.test(html),
        'подписи ролей не те: ' + html);
});

check('R118: выбран ТЕКУЩИЙ role, пустая роль = «не задана»', function () {
    const mainHtml = MB.roleSelect(LLAMA);
    assert.ok(/<option value="main" selected>/.test(mainHtml), 'роль main не отмечена: ' + mainHtml);
    const donorHtml = MB.roleSelect(DONOR);
    assert.ok(/<option value="donor" selected>/.test(donorHtml), 'роль donor не отмечена: ' + donorHtml);
    const noneHtml = MB.roleSelect({ id: 'b', type: 'llama_cpp' });
    assert.ok(/<option value="" selected>/.test(noneHtml), 'пустая роль должна быть «не задана»: ' + noneHtml);
});

check('R118: у image-воркера селектора роли нет (раскладка к нему не относится)', function () {
    assert.strictEqual(MB.roleSelect(IMAGE), '', 'у image-воркера появился селектор роли');
});

check('R118: строка бэкенда содержит селектор роли вместе с именем', function () {
    const row = MB.renderRow(LLAMA);
    assert.ok(/cppworker-gpu-bundled-agent/.test(row), 'в строке нет имени бэкенда');
    assert.ok(/backend-role-select/.test(row), 'в строке нет селектора роли: ' + row);
    const imgRow = MB.renderRow(IMAGE);
    assert.ok(!/backend-role-select/.test(imgRow), 'у image-воркера в строке появился селектор роли');
});

check('R118: смена роли уходит в Api.setBackendRole(bid, role)', function () {
    const calls = [];
    let changeHandler = null;
    const sel = {
        value: 'donor',
        getAttribute: function () { return 'cppworker-cyborg-bundled-agent'; },
        addEventListener: function (ev, fn) { if (ev === 'change') changeHandler = fn; }
    };
    stubTbody.querySelectorAll = function (q) {
        return q === '.backend-role-select' ? [sel] : [];
    };
    global.Api = {
        setBackendRole: function (id, role) { calls.push({ id: id, role: role }); return Promise.resolve(true); }
    };

    MB.renderTable([DONOR]);
    assert.ok(changeHandler, 'обработчик change не навешен на селектор роли');
    changeHandler();
    assert.deepStrictEqual(calls, [{ id: 'cppworker-cyborg-bundled-agent', role: 'donor' }],
        'смена роли не ушла в Api.setBackendRole: ' + JSON.stringify(calls));
});

console.log('\nOK: ' + checks + ' checks passed');
