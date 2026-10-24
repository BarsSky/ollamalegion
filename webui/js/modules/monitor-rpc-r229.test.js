// monitor-rpc-r229.test.js — R229 (2026-10-22).
//
// Запуск: node webui/js/modules/monitor-rpc-r229.test.js
//
// ЖИВОЙ ДЕФЕКТ, который фиксирует тест. Оператор выключал раскладку RPC (тумблер
// «выкл» + «Применить»), воркер это применял, а балансерский гард через ~15 с
// возвращал rpcEnabled=true (роль главного + живой донор = «включить»). В панели
// это выглядело как «тумблер сам включается обратно».
//
// Что проверяем:
//   1. у главного (llama_cpp) есть чекбокс RPC, кнопка «Применить» и кнопка «авто»,
//      а рядом — состояние «авто (по ролям)»;
//   2. решение оператора видно по состоянию: «вкл оператором» / «выкл оператором»,
//      чекбокс отражает ЖЕЛАНИЕ, а не роль;
//   3. донору и image-воркеру тумблер не показываем (донор — источник слоёв);
//   4. «Применить» уходит в Api.setBackendRPCLayout(id, 'on'|'off') по состоянию
//      чекбокса, «авто» — в Api.setBackendRPCLayout(id, 'auto').

'use strict';

const assert = require('assert');

global.window = global;
global.MonitorApp = {
    esc: function (s) { return String(s === null || s === undefined ? '' : s); },
    fmtDur: function () { return '-'; }
};
global.I18N = { t: function (key) { return key; } };

const makeEl = function () {
    return {
        innerHTML: '', textContent: '', value: '', checked: false, style: {},
        classList: { add() {}, remove() {}, contains() { return false; } },
        addEventListener() {}, querySelectorAll() { return []; },
        setAttribute() {}, getAttribute() { return null; }, appendChild() {}
    };
};
const stubTbody = {
    innerHTML: '',
    querySelectorAll: function () { return []; },
    querySelector: function () { return null; }
};
global.document = {
    querySelector: function (sel) { return sel === '#backendsTable tbody' ? stubTbody : null; },
    getElementById: function () { return null; },
    querySelectorAll: function () { return []; },
    createElement: function () { return makeEl(); },
    addEventListener: function () {}
};
global.Utils = { showToast: function () {} };

const fs = require('fs');
const path = require('path');
const NodeModule = require('module');
const SRC = path.join(__dirname, 'monitor-backends.js');
const m = new NodeModule(SRC, module);
m._compile(fs.readFileSync(SRC, 'utf8') + '\nmodule.exports = MonitorBackends;', SRC);
const MB = m.exports;
global.window.MonitorBackends = MB;

const MAIN = { id: 'cppworker-gpu-bundled-agent', type: 'llama_cpp', role: 'main' };
const DONOR = { id: 'cppworker-cyborg-bundled-agent', type: 'llama_cpp', role: 'donor' };
const IMAGE = { id: 'imageworker', type: 'image_cpp', role: 'main' };

let checks = 0;
function check(name, fn) {
    const r = fn();
    if (r && typeof r.then === 'function') {
        return r.then(function () { checks++; console.log('  ✓ ' + name); });
    }
    checks++;
    console.log('  ✓ ' + name);
    return Promise.resolve();
}

const tick = function () { return new Promise(function (r) { setTimeout(r, 0); }); };

(async function main() {
    await check('R229: у главного есть тумблер RPC, «Применить», «авто» и состояние', function () {
        const html = MB.renderRow(MAIN);
        assert.ok(/backend-rpc-enabled/.test(html), 'нет чекбокса RPC: ' + html);
        assert.ok(/backend-rpc-apply/.test(html), 'нет кнопки «Применить»: ' + html);
        assert.ok(/backend-rpc-auto/.test(html), 'нет кнопки «авто»: ' + html);
        assert.ok(/monitor\.rpc\.state\.auto/.test(html), 'нет состояния «авто»: ' + html);
    });

    await check('R229: решение оператора видно состоянием, чекбокс = желание', function () {
        const on = MB.roleSelect({ id: 'b', type: 'llama_cpp', role: 'main', rpcEnabledOverride: 'on' });
        assert.ok(/backend-rpc-enabled[^>]* checked/.test(on), 'при mode=on чекбокс не отмечен: ' + on);
        assert.ok(/monitor\.rpc\.state\.on/.test(on), 'нет состояния «вкл оператором»: ' + on);

        const off = MB.roleSelect({ id: 'b', type: 'llama_cpp', role: 'main', rpcEnabledOverride: 'off' });
        assert.ok(!/backend-rpc-enabled[^>]* checked/.test(off), 'при mode=off чекбокс отмечен: ' + off);
        assert.ok(/monitor\.rpc\.state\.off/.test(off), 'нет состояния «выкл оператором»: ' + off);

        // Авто у главного: желание = по роли (main → включено), состояние «авто».
        const auto = MB.roleSelect({ id: 'b', type: 'llama_cpp', role: 'main' });
        assert.ok(/backend-rpc-enabled[^>]* checked/.test(auto), 'в авто у главного чекбокс не отражает роль: ' + auto);
        assert.ok(/monitor\.rpc\.state\.auto/.test(auto), 'нет состояния «авто (по ролям)»: ' + auto);
    });

    await check('R229: донору и image-воркеру тумблер не показываем', function () {
        assert.ok(!/backend-rpc-enabled/.test(MB.renderRow(DONOR)), 'у донора появился тумблер RPC');
        assert.ok(!/backend-rpc-enabled/.test(MB.renderRow(IMAGE)), 'у image-воркера появился тумблер RPC');
    });

    await check('R229: «Применить» отправляет режим по состоянию чекбокса', async function () {
        const calls = [];
        let apply = null;
        let auto = null;
        const btnApply = {
            getAttribute: function () { return 'cppworker-gpu-bundled-agent'; },
            addEventListener: function (ev, fn) { if (ev === 'click') apply = fn; }
        };
        const btnAuto = {
            getAttribute: function () { return 'cppworker-gpu-bundled-agent'; },
            addEventListener: function (ev, fn) { if (ev === 'click') auto = fn; }
        };
        stubTbody.querySelectorAll = function (q) {
            if (q === '.backend-rpc-apply') return [btnApply];
            if (q === '.backend-rpc-auto') return [btnAuto];
            return [];
        };
        stubTbody.querySelector = function (q) {
            if (q === '.backend-rpc-enabled[data-backend="cppworker-gpu-bundled-agent"]') {
                return { checked: false };
            }
            return null;
        };
        global.Api = {
            setBackendRPCLayout: function (id, mode) {
                calls.push({ id: id, mode: mode });
                return Promise.resolve({ ok: true, body: {} });
            }
        };

        MB.renderTable([MAIN]);
        assert.ok(apply, 'обработчик «Применить» не навешен');
        assert.ok(auto, 'обработчик «авто» не навешен');

        apply();
        await tick();
        assert.deepStrictEqual(calls, [{ id: 'cppworker-gpu-bundled-agent', mode: 'off' }],
            '«Применить» не отправил off по чекбоксу: ' + JSON.stringify(calls));

        auto();
        await tick();
        assert.deepStrictEqual(calls, [
            { id: 'cppworker-gpu-bundled-agent', mode: 'off' },
            { id: 'cppworker-gpu-bundled-agent', mode: 'auto' }
        ], '«авто» не отправил auto: ' + JSON.stringify(calls));
    });

    console.log('\nВсе проверки пройдены: ' + checks);
})().catch(function (err) {
    console.error('FAIL: ' + (err && err.stack ? err.stack : err));
    process.exit(1);
});
