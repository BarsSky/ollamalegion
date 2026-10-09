// topology-layout.test.js — R91 (2026-10-08).
//
// Запуск: node webui/js/monitor/topology-layout.test.js
//
// ЖАЛОБА ОПЕРАТОРА: «в мониторе плитки отображающие клиентов и бэкендов слишком
// сильно наплывают друг на друга скрывая информацию» (скриншот: колонка из
// двенадцати клиентских плиток, наезжающих одна на другую, и красная плитка
// share-probe поверх зелёных бэкендов).
//
// ПРИЧИНА. Раскладка узлов считалась так:
//
//	v = h - marginY*2;
//	шаг = max(minSpacing, v / (cnt - 1));   // minSpacing = 30
//
// а плитки рисуются высотой 44 px (клиент) и 52 px (бэкенд). То есть НИЖНЯЯ
// граница шага (30) была меньше высоты плитки: как только узлов становилось
// больше, чем влезает, шаг упирался в 30 и плитки наезжали друг на друга.
//
// Тест фиксирует инвариант: шаг НИКОГДА не меньше «высота плитки + зазор», а
// лишние узлы не рисуются — вместо них счётчик «+N» (nLayout().hidden).

'use strict';

const assert = require('assert');

// --- минимальное окружение для state.js -------------------------------------
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
// navigator в Node 22 — геттер только для чтения, подменять не нужно.

require('./state.js');
const MA = global.window.MonitorApp;
assert.ok(MA && typeof MA.nLayout === 'function', 'MonitorApp.nLayout должен существовать');

const GAP = MA.GEOM.nodeGap;
const H = { session: MA.GEOM.nodeH.session, backend: MA.GEOM.nodeH.backend };

let checks = 0;
function check(name, fn) {
    fn();
    checks++;
    console.log('  ✓ ' + name);
}

// Готовим состояние: h — высота канваса, sess/back — число узлов.
function setup(h, sess, back) {
    MA.topo.h = h;
    MA.topo.sessions = [];
    for (let i = 0; i < sess; i++) MA.topo.sessions.push({ id: 's' + i, lastRequestAt: null });
    MA.topo.backends = [];
    for (let i = 0; i < back; i++) MA.topo.backends.push({ id: 'b' + i, status: 'healthy' });
}

// Живой случай оператора: канвас ~302 px, 12 клиентов и 6 бэкендов
// (скриншот 1186x302).
check('живой случай (302 px, 12 клиентов): шаг ≥ высоты плитки + зазор', function () {
    setup(302, 12, 6);
    const L = MA.nLayout('session');
    assert.ok(L.spacing >= H.session + GAP,
        'шаг ' + L.spacing.toFixed(1) + ' меньше высоты плитки ' + H.session + ' + зазор — плитки снова наедут');
    // Соседние плитки не пересекаются по вертикали.
    for (let i = 1; i < L.visible; i++) {
        const gap = MA.nY('session', i) - MA.nY('session', i - 1);
        assert.ok(gap >= H.session,
            'плитки ' + (i - 1) + ' и ' + i + ' пересекаются: зазор ' + gap.toFixed(1) + ' px < ' + H.session);
    }
});

check('то же для бэкендов (плитка 52 px)', function () {
    setup(302, 12, 6);
    const L = MA.nLayout('backend');
    assert.ok(L.spacing >= H.backend + GAP, 'шаг ' + L.spacing.toFixed(1) + ' меньше высоты плитки бэкенда');
    for (let i = 1; i < L.visible; i++) {
        const gap = MA.nY('backend', i) - MA.nY('backend', i - 1);
        assert.ok(gap >= H.backend, 'плитки бэкендов пересекаются: ' + gap.toFixed(1) + ' px');
    }
});

check('не поместившиеся узлы посчитаны, а не нарисованы поверх', function () {
    setup(302, 12, 6);
    const Ls = MA.nLayout('session'), Lb = MA.nLayout('backend');
    assert.strictEqual(Ls.count, 12);
    assert.ok(Ls.visible < 12, 'на 302 px все 12 плиток не могут влезть без наложения');
    assert.strictEqual(Ls.visible + Ls.hidden, 12, 'visible + hidden должны давать общее число');
    assert.strictEqual(Lb.visible + Lb.hidden, 6, 'visible + hidden должны давать общее число (бэкенды)');
});

check('узлы не выходят за пределы канваса', function () {
    [200, 302, 480, 700, 1000].forEach(function (h) {
        setup(h, 25, 7);
        ['session', 'backend'].forEach(function (type) {
            const L = MA.nLayout(type);
            if (L.visible === 0) return;
            const half = H[type] / 2;
            const top = MA.nY(type, 0) - half;
            const bottom = MA.nY(type, L.visible - 1) + half;
            assert.ok(top >= -1, type + ': верхняя плитка обрезана (top=' + top.toFixed(1) + ', h=' + h + ')');
            assert.ok(bottom <= h + 1, type + ': нижняя плитка вылезла за канвас (bottom=' + bottom.toFixed(1) + ', h=' + h + ')');
        });
    });
});

check('когда места хватает — прятать нечего, колонка занимает всю полосу', function () {
    setup(900, 4, 3);
    const L = MA.nLayout('session');
    assert.strictEqual(L.hidden, 0, 'на 900 px четыре клиента должны поместиться');
    assert.ok(L.offset >= 0, 'отступ не может быть отрицательным');
    assert.ok(L.spacing >= H.session + GAP);
    // Колонка занимает ровно доступную полосу: ровно то, для чего считался шаг.
    const span = MA.nY('session', L.visible - 1) - MA.nY('session', 0);
    const av = 900 - MA.GEOM.marginY * 2;
    assert.ok(span <= av + 1, 'колонка вылезла из полосы: ' + span.toFixed(1) + ' > ' + av);
});

check('на высоком канвасе помещается заметно больше узлов', function () {
    setup(302, 12, 6);
    const low = MA.nLayout('session').visible;
    setup(900, 12, 6);
    const high = MA.nLayout('session').visible;
    assert.ok(high > low, 'высота канваса должна влиять на вместимость (' + low + ' → ' + high + ')');
    assert.strictEqual(high, 12, 'на 900 px все 12 клиентов обязаны поместиться');
});

check('сотня узлов не рисуется целиком (предел maxNodes)', function () {
    setup(4000, 100, 100);
    assert.ok(MA.nLayout('session').visible <= MA.GEOM.maxNodes,
        'рисуется больше ' + MA.GEOM.maxNodes + ' плиток — картинка нечитаема');
    assert.strictEqual(MA.nLayout('session').hidden, 100 - MA.nLayout('session').visible);
});

check('пустое состояние не ломает раскладку', function () {
    setup(0, 0, 0);
    const L = MA.nLayout('session');
    assert.ok(L.visible >= 1 && isFinite(MA.nY('session', 0)), 'нулевая высота не должна давать NaN');
});

console.log('\nOK: ' + checks + ' checks passed');
console.log('  (высота 302 px: клиентов видно ' + (setup(302, 12, 6), MA.nLayout('session').visible) +
    ' из 12, бэкендов ' + MA.nLayout('backend').visible + ' из 6; на 900 px — ' +
    (setup(900, 12, 6), MA.nLayout('session').visible) + ' из 12)');
