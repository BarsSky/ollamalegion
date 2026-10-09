// topology-layout.test.js — раскладка колонок клиентов/бэкендов на канвасе.
//
// Запуск: node webui/js/monitor/topology-layout.test.js
//
// ИСТОРИЯ.
// R91 (2026-10-08): жалоба «плитки клиентов и бэкендов наплывают друг на друга».
// Причина — шаг мог сжиматься до minSpacing=30 при высоте плитки 44/52. Тогда
// сделали «шаг ≥ плитка + зазор» И спрятали лишние узлы за плашкой «+N», вписывая
// колонку в ТЕКУЩУЮ высоту канваса.
//
// R92 (2026-10-09): жалоба «расширяется поле свободно, не пытаясь уместить всех в
// области отображения». Живой замер до правки: окно 1440x900, 14 бэкендов и 12
// клиентов → canvas.h=398, section.overflowY=visible, backendLayout={visible:5,
// hidden:9}, sessionLayout={visible:6, hidden:6}. То есть 15 из 26 плиток были
// скрыты, хотя место в окне есть.
//
// НОВЫЙ КОНТРАКТ (его и фиксирует тест):
//   * шаг ВСЕГДА равен «высота плитки + зазор» и никогда не сжимается;
//   * все узлы рисуются (hidden=0), пока их число не превышает предохранители;
//   * нужную высоту полотна сообщает MonitorApp.columnNeedH(type) — владелец
//     канваса (canvas-topology.js: layoutCanvas) растит полотно до неё, а полоса
//     топологии прокручивается (.topo-viewport{overflow-y:auto});
//   * прятать узлы («+N») разрешено только за предохранителями maxNodes /
//     maxCanvasH — от патологического кластера, а не от размера окна.

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

require('./state.js');
const MA = global.window.MonitorApp;
assert.ok(MA && typeof MA.nLayout === 'function', 'MonitorApp.nLayout должен существовать');
assert.ok(typeof MA.columnNeedH === 'function', 'MonitorApp.columnNeedH должен существовать');

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

// --- 1. Шаг не сжимается ни при какой высоте окна ---------------------------
check('шаг ≥ высота плитки + зазор при любом окне (44/52 px плитки)', function () {
    [120, 200, 302, 480, 700, 1000, 4000].forEach(function (h) {
        [0, 1, 3, 12, 40].forEach(function (n) {
            setup(h, n, n);
            ['session', 'backend'].forEach(function (type) {
                const L = MA.nLayout(type);
                assert.ok(L.spacing >= H[type] + GAP,
                    type + ': шаг ' + L.spacing.toFixed(1) + ' меньше плитки ' + H[type] + ' + зазор (h=' + h + ', n=' + n + ')');
                for (let i = 1; i < L.visible; i++) {
                    const gap = MA.nY(type, i) - MA.nY(type, i - 1);
                    assert.ok(gap >= H[type],
                        type + ': плитки ' + (i - 1) + ' и ' + i + ' пересекаются (зазор ' + gap.toFixed(1) + ')');
                }
            });
        });
    });
});

// --- 2. Старое «уместить в окно и спрятать» больше не работает --------------
check('узлы не прячутся из-за размера окна (R92: было visible 5 из 14)', function () {
    setup(398, 12, 14);              // ровно живой замер жалобы
    const Ls = MA.nLayout('session'), Lb = MA.nLayout('backend');
    assert.strictEqual(Ls.count, 12);
    assert.strictEqual(Lb.count, 14);
    assert.strictEqual(Ls.hidden, 0, 'на 398 px спрятано ' + Ls.hidden + ' клиентов — поле обязано вырасти');
    assert.strictEqual(Lb.hidden, 0, 'на 398 px спрятано ' + Lb.hidden + ' бэкендов — поле обязано вырасти');
    assert.strictEqual(Ls.visible, 12);
    assert.strictEqual(Lb.visible, 14);
});

// --- 3. columnNeedH: сколько нужно полотну и что все узлы в него влезают ----
check('columnNeedH вмещает все узлы с запасом marginY', function () {
    [1, 2, 7, 14, 40].forEach(function (n) {
        setup(120, n, n);
        ['session', 'backend'].forEach(function (type) {
            const need = MA.columnNeedH(type);
            const L = MA.nLayout(type);
            assert.strictEqual(L.contentH, need, type + ': contentH раскладки ≠ columnNeedH');
            // Полотно вырастает до need — проверяем, что плитки внутри.
            setup(need, n, n);
            const L2 = MA.nLayout(type);
            const bottom = MA.nY(type, L2.visible - 1) + H[type] / 2;
            assert.ok(bottom <= need + 1,
                type + ': нижняя плитка (' + bottom.toFixed(1) + ') вылезает за полотно ' + need);
            const top = MA.nY(type, 0) - H[type] / 2;
            assert.ok(top >= 0, type + ': верхняя плитка обрезана (top=' + top.toFixed(1) + ')');
        });
    });
});

check('нужная высота растёт с числом узлов и больше окна', function () {
    setup(398, 1, 1);
    const h1 = MA.columnNeedH('backend');
    setup(398, 1, 14);
    const h14 = MA.columnNeedH('backend');
    assert.ok(h14 > h1, 'columnNeedH не вырос: ' + h1 + ' → ' + h14);
    assert.ok(h14 > 398, 'для 14 бэкендов нужно больше высоты окна (398): ' + h14);
    // Ровно (n-1) шагов + плитка + поля.
    const step = H.backend + GAP;
    assert.strictEqual(h14, MA.GEOM.marginY * 2 + 13 * step + H.backend);
});

// --- 4. Предохранители: прятать можно только за них -------------------------
check('реалистичный кластер (60 узлов) виден целиком', function () {
    setup(398, 60, 60);
    ['session', 'backend'].forEach(function (type) {
        const L = MA.nLayout(type);
        assert.strictEqual(L.hidden, 0, type + ': 60 узлов обязаны быть видны все (hidden=' + L.hidden + ')');
        assert.ok(MA.columnNeedH(type) <= MA.GEOM.maxCanvasH,
            type + ': 60 узлов не влезают в maxCanvasH — поднимите предел');
    });
});

check('за предохранителями прячем, но не больше maxNodes и в пределах maxCanvasH', function () {
    const over = MA.GEOM.maxNodes + 25;
    setup(398, over, over);
    const L = MA.nLayout('session');
    assert.strictEqual(L.visible + L.hidden, over, 'visible + hidden должны давать общее число');
    assert.ok(L.visible <= MA.GEOM.maxNodes, 'видимых больше maxNodes: ' + L.visible);
    assert.ok(L.hidden > 0, 'за предохранителями hidden обязан быть > 0');
    const need = MA.columnNeedH('session');
    assert.ok(need <= MA.GEOM.maxCanvasH, 'нужная высота ' + need + ' больше maxCanvasH ' + MA.GEOM.maxCanvasH);
    assert.ok(L.chipY <= need, 'плашка «+N» (' + L.chipY.toFixed(1) + ') вылезла за полотно ' + need);
});

check('предел высоты полотна соблюдён (maxCanvasH)', function () {
    setup(398, 2000, 2000);
    ['session', 'backend'].forEach(function (type) {
        const need = MA.columnNeedH(type);
        assert.ok(need <= MA.GEOM.maxCanvasH, type + ': columnNeedH ' + need + ' больше maxCanvasH ' + MA.GEOM.maxCanvasH);
    });
});

// --- 5. Пустое состояние и центрирование ------------------------------------
check('пустое состояние не ломает раскладку', function () {
    setup(0, 0, 0);
    const L = MA.nLayout('session');
    assert.ok(L.visible >= 1 && isFinite(MA.nY('session', 0)), 'нулевая высота не должна давать NaN');
});

check('когда полотно выше контента — колонка центрируется', function () {
    setup(900, 3, 3);
    const L = MA.nLayout('session');
    assert.ok(L.offset > 0, 'мало узлов на высоком полотне → колонка должна центрироваться (offset=' + L.offset + ')');
    const span = MA.nY('session', L.visible - 1) - MA.nY('session', 0);
    assert.ok(span <= 900 - MA.GEOM.marginY * 2 + 1, 'колонка вылезла из полосы: ' + span);
});

console.log('\nOK: ' + checks + ' checks passed');
setup(398, 12, 14);
console.log('  (живой случай: 12 клиентов — видно ' + MA.nLayout('session').visible +
    ', 14 бэкендов — видно ' + MA.nLayout('backend').visible +
    '; нужная высота полотна ' + Math.max(MA.columnNeedH('session'), MA.columnNeedH('backend')) + ' px)');
