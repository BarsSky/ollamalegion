// monitor-mobile-layout.test.js — R238 (2026-10-25): геометрия монитора на телефоне.
//
// ЖАЛОБА: «необходимо оптимизировать webui под маленький экран смартфона, сейчас
// элементы накладываются кучей». В мониторе это было буквально так: при 390 px
// клиентская плитка (12..132) и плитки бэкендов (w-140..w-12) наезжали на блок
// балансера (w/2 ± 90), потому что clientX/backendMargin/bw были фиксированными.
//
// КОНТРАКТ (его и фиксирует тест): GEOM.applyResponsive(w) при любой ширине
// оставляет зазоры — линия клиента заканчивается ЛЕВЕЕ блока балансера, а плитки
// бэкендов начинаются ПРАВЕЕ него; на десктопе (>=720 px) значения не меняются.
//
// Запуск: node webui/js/modules/monitor-mobile-layout.test.js

'use strict';

const assert = require('assert');

// --- минимальное окружение для state.js (как в topology-layout.test.js) -----
global.window = global;
global.window.WEBUI_CONFIG = {};
global.localStorage = { getItem() { return null; }, setItem() {}, removeItem() {} };
global.document = {
    getElementById() { return null; },
    addEventListener() {},
    removeEventListener() {},
    documentElement: { getAttribute() { return null; } },
    querySelectorAll() { return []; },
    createElement() { return { style: {}, classList: { add() {}, remove() {} }, appendChild() {} }; },
};
global.window.location = { search: '', origin: 'http://127.0.0.1:18083', hash: '' };
global.location = global.window.location;
global.window.addEventListener = function () {};
global.window.removeEventListener = function () {};
global.addEventListener = global.window.addEventListener;
global.removeEventListener = global.window.removeEventListener;

require('../monitor/state.js');
const MA = global.window.MonitorApp;
assert.ok(MA && MA.GEOM && typeof MA.GEOM.applyResponsive === 'function',
    'MonitorApp.GEOM.applyResponsive должен существовать');

let checks = 0;
function check(name, fn) {
    fn();
    checks++;
    console.log('  ✓ ' + name);
}

const G = MA.GEOM;

check('десктоп (>=720 px) не меняет геометрию', function () {
    [720, 1024, 1440, 1920].forEach(function (w) {
        G.applyResponsive(w);
        assert.strictEqual(G.clientX, 140, 'clientX на десктопе (w=' + w + ')');
        assert.strictEqual(G.backendMargin, 140, 'backendMargin на десктопе (w=' + w + ')');
        assert.strictEqual(G.bw, 180, 'bw на десктопе (w=' + w + ')');
    });
});

check('телефон: клиентская ветка не заезжает на балансер', function () {
    [320, 360, 390, 414, 430, 600, 719].forEach(function (w) {
        G.applyResponsive(w);
        const balInX = G.balInX(w);
        assert.ok(G.clientX <= balInX - 8,
            'w=' + w + ': линия клиента (' + G.clientX + ') должна кончаться левее балансера (' + balInX + ')');
        const tileW = G.clientX - 20;
        assert.ok(tileW >= 64, 'w=' + w + ': клиентская плитка слишком узкая (' + tileW + ')');
    });
});

check('телефон: плитки бэкендов не заезжают на балансер', function () {
    [320, 360, 390, 414, 430, 600, 719].forEach(function (w) {
        G.applyResponsive(w);
        const balOutX = G.balOutX(w);
        const backendX = w - G.backendMargin;
        assert.ok(backendX >= balOutX + 8,
            'w=' + w + ': плитки бэкендов начинаются (' + backendX + ') левее правой границы балансера (' + balOutX + ')');
        const tileW = G.backendMargin - 12;
        assert.ok(tileW >= 72, 'w=' + w + ': плитка бэкенда слишком узкая (' + tileW + ')');
    });
});

check('телефон: балансер не вырождается и остаётся по центру', function () {
    [320, 360, 390, 414, 430, 600, 719].forEach(function (w) {
        G.applyResponsive(w);
        assert.ok(G.bw >= 96 && G.bw <= 180, 'w=' + w + ': ширина балансера вне [96, 180]: ' + G.bw);
        const balInX = G.balInX(w), balOutX = G.balOutX(w);
        assert.ok(Math.abs((balInX + balOutX) / 2 - w / 2) < 0.001,
            'w=' + w + ': балансер должен быть по центру полотна');
    });
});

check('переключение ширины обратимо (телефон -> десктоп)', function () {
    G.applyResponsive(390);
    assert.notStrictEqual(G.clientX, 140);
    G.applyResponsive(1600);
    assert.strictEqual(G.clientX, 140);
    assert.strictEqual(G.backendMargin, 140);
    assert.strictEqual(G.bw, 180);
});

console.log('\nOK: ' + checks + ' checks passed');
