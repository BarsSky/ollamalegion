// layout-panel-r118.test.js — R118.4 (2026-10-14).
//
// Запуск: node webui/js/monitor/layout-panel-r118.test.js
//
// ЖАЛОБА ОПЕРАТОРА: «не увидел в webui как настроить RPC через него — идея была
// управлять бэкендами, связывая их вместе, определяя кто главный а кто донор
// через веб-панель».
//
// ПРИЧИНА (найдена в коде): селектор роли из R118 жил в
// js/modules/monitor-backends.js, а таблицу бэкендов на странице «Мониторинг»
// рисует js/monitor/ui-renderer.js. Плюс в js/monitor/api.js не было
// setBackendRole. В живой панели управления раскладкой не было вовсе.
//
// Этот тест фиксирует ЖИВОЙ путь:
//   1. ячейка строки содержит селектор роли с текущим значением и состояние
//      раскладки (включена / деградация с причиной / узел / локально);
//   2. панель «Раскладка слоёв (RPC)» показывает выбранного главного и доноров;
//   3. «Применить раскладку» отправляет роли ВСЕМ llama.cpp-бэкендам: выбранному
//      главному main, отмеченным донорам donor, остальным standalone (иначе у
//      бэкенда, с которого роль сняли, осталась бы прежняя раскладка);
//   4. «Только локально» переводит все llama.cpp-бэкенды в standalone;
//   5. image-бэкенд раскладкой не управляется.

'use strict';

const assert = require('assert');

global.window = global;

const T = {
    'monitor.role.none': '— не задана —',
    'monitor.role.standalone': 'самодостаточный',
    'monitor.role.main': 'главный (раскладка)',
    'monitor.role.donor': 'донор (узел)',
    'monitor.role.title': 'Роль бэкенда в раскладке слоёв',
    'monitor.layout.title': '🧩 Раскладка слоёв (RPC)',
    'monitor.layout.main': 'Главный (мастер раскладки)',
    'monitor.layout.donors': 'Доноры (узлы раскладки)',
    'monitor.layout.on': 'раскладка включена',
    'monitor.layout.off': 'раскладка выключена',
    'monitor.layout.nodesShort': 'узл.',
    'monitor.layout.link': 'канал',
    'monitor.layout.mbps': 'Мбит/с',
    'monitor.layout.degradedShort': 'раскладка не в полном составе',
    'monitor.layout.donorShort': 'узел раскладки',
    'monitor.layout.localShort': 'локально',
    'monitor.layout.noMain': '— не выбран —',
    'monitor.layout.statusNodes': 'узлы',
    'monitor.layout.badgeOn': 'включена',
    'monitor.layout.statusNoMain': 'Главный не выбран: каждая машина считает модель сама.'
};
global.I18N = { t: function (k) { return Object.prototype.hasOwnProperty.call(T, k) ? T[k] : k; } };
global.MonitorApp = {
    esc: function (s) { return String(s === null || s === undefined ? '' : s); },
    T: function (k) { return Object.prototype.hasOwnProperty.call(T, k) ? T[k] : k; },
    API_BASE: ''
};

// ---- DOM-заглушка ---------------------------------------------------------
function makeEl(id) {
    const el = {
        id: id, innerHTML: '', textContent: '', value: '', style: {}, dataset: {},
        checked: false,
        classList: { add() {}, remove() {}, contains() { return false; } },
        listeners: {},
        addEventListener: function (ev, fn) { (el.listeners[ev] = el.listeners[ev] || []).push(fn); },
        querySelectorAll: function () { return []; },
        getAttribute: function (k) { return el[k] || null; },
        setAttribute: function (k, v) { el[k] = v; },
        appendChild: function () {}
    };
    return el;
}
const elements = {
    layoutMainSelect: makeEl('layoutMainSelect'),
    layoutDonorList: makeEl('layoutDonorList'),
    layoutStatus: makeEl('layoutStatus'),
    layoutStateBadge: makeEl('layoutStateBadge'),
    layoutApplyBtn: makeEl('layoutApplyBtn'),
    layoutLocalBtn: makeEl('layoutLocalBtn')
};
global.document = {
    readyState: 'complete',
    getElementById: function (id) { return elements[id] || null; },
    querySelector: function () { return null; },
    querySelectorAll: function () { return []; },
    createElement: function () { return makeEl(''); },
    addEventListener: function () {}
};
global.Utils = { showToast: function () {} };

// Подставной транспорт: пишем, какие роли ушли на балансер.
const roleCalls = [];
let backendsPayload = [];
global.MonitorApiXhr = {
    fetchBackends: function () { return Promise.resolve({ backends: backendsPayload }); },
    setBackendRole: function (id, role) { roleCalls.push(id + '=' + role); return Promise.resolve(true); }
};

require('./layout-panel.js');
const ML = global.MonitorLayout;
assert.ok(ML && typeof ML.rowLayoutCell === 'function', 'MonitorLayout должен быть опубликован');

// Живые данные стенда: мастер с включённой раскладкой на узел 192.0.2.10.
const MAIN = {
    id: 'cppworker-gpu-bundled-agent', backendType: 'llama_cpp', role: 'main',
    rpcLayout: { enabled: true, degraded: false, servers: ['192.0.2.10:50052'] },
    rpcLink: { mbps: 889, measured_at: '2026-10-14' }
};
const DONOR = { id: 'cppworker-cyborg-bundled-agent', backendType: 'llama_cpp', role: 'donor' };
const IMG = { id: 'imageworker', backendType: 'image_cpp', role: '' };

let checks = 0;
async function check(name, fn) {
    await fn();
    checks++;
    console.log('  ✓ ' + name);
}

(async function () {
    await check('R118.4: ячейка строки — селектор роли с текущим значением и чип состояния', function () {
        const cell = ML.rowLayoutCell(MAIN);
        assert.ok(/<select class="backend-role-select"/.test(cell), 'нет селектора роли: ' + cell);
        assert.ok(/data-backend="cppworker-gpu-bundled-agent"/.test(cell), 'селектор не привязан к бэкенду: ' + cell);
        assert.ok(/<option value="main" selected>/.test(cell), 'текущая роль не выбрана: ' + cell);
        assert.ok(/раскладка включена/.test(cell), 'нет состояния «включена»: ' + cell);
        assert.ok(/1 узл\./.test(cell), 'нет числа узлов: ' + cell);
    });

    await check('R118.4: деградация раскладки видна прямо в строке с причиной', function () {
        const main = Object.assign({}, MAIN, {
            rpcLayout: {
                enabled: false, degraded: true,
                reason: 'узлы раскладки недоступны (192.0.2.10:50052): раскладка выключена, загрузка идёт локально'
            }
        });
        const cell = ML.rowLayoutCell(main);
        assert.ok(/badge-orange/.test(cell), 'нет предупреждающего бейджа: ' + cell);
        assert.ok(/192\.0\.2\.10:50052/.test(cell), 'нет причины деградации: ' + cell);
    });

    await check('R118.4: донор и самодостаточный бэкенд подписаны по-разному', function () {
        assert.ok(/узел раскладки/.test(ML.rowLayoutCell(DONOR)), 'донор не помечен');
        assert.ok(/локально/.test(ML.rowLayoutCell({ id: 'x', backendType: 'llama_cpp', role: 'standalone' })),
            'самодостаточный не помечен как локальный');
    });

    await check('R118.4: image-бэкенд раскладкой не управляется', function () {
        const cell = ML.rowLayoutCell(IMG);
        assert.ok(cell.indexOf('backend-role-select') === -1, 'у image-бэкенда не должно быть селектора роли: ' + cell);
        assert.strictEqual(ML.isLlamaBackend(IMG), false, 'image_cpp не llama-бэкенд');
    });

    await check('R118.4: панель показывает главного и отмечает доноров', function () {
        backendsPayload = [MAIN, DONOR, IMG];
        ML._state.backends = backendsPayload;
        ML.renderPanel();
        assert.ok(/<option value="cppworker-gpu-bundled-agent" selected>/.test(elements.layoutMainSelect.innerHTML),
            'главный не выбран в списке: ' + elements.layoutMainSelect.innerHTML);
        assert.ok(/data-backend="cppworker-cyborg-bundled-agent" checked/.test(elements.layoutDonorList.innerHTML),
            'донор не отмечен: ' + elements.layoutDonorList.innerHTML);
        assert.ok(elements.layoutDonorList.innerHTML.indexOf('data-backend="cppworker-gpu-bundled-agent"') === -1,
            'главный не должен предлагаться сам себе донором: ' + elements.layoutDonorList.innerHTML);
        assert.ok(/раскладка включена/.test(elements.layoutStatus.innerHTML), 'нет состояния в статусе');
        assert.ok(/889/.test(elements.layoutStatus.innerHTML), 'нет замера канала в статусе');
        assert.strictEqual(elements.layoutStateBadge.textContent, T['monitor.layout.badgeOn']);
    });

    await check('R118.4: расчёт ролей — остальные бэкенды ЯВНО самодостаточные', function () {
        ML._state.backends = [MAIN, DONOR, IMG];
        const roles = ML.rolesForSelection('cppworker-gpu-bundled-agent', ['cppworker-cyborg-bundled-agent']);
        assert.strictEqual(roles['cppworker-gpu-bundled-agent'], 'main');
        assert.strictEqual(roles['cppworker-cyborg-bundled-agent'], 'donor');
        assert.strictEqual(roles['imageworker'], undefined, 'image-бэкенд не должен получать роль раскладки');
        // Бэкенд, который был донором, но снят с выбора → standalone: иначе у него
        // осталась бы прежняя раскладка (живой случай: главный стал донором).
        const roles2 = ML.rolesForSelection('cppworker-gpu-bundled-agent', []);
        assert.strictEqual(roles2['cppworker-cyborg-bundled-agent'], 'standalone');
    });

    await check('R118.4: «Применить раскладку» отправляет роли всем llama.cpp-бэкендам', function () {
        roleCalls.length = 0;
        backendsPayload = [MAIN, DONOR, IMG];
        ML._state.backends = backendsPayload;
        ML.renderPanel();
        elements.layoutMainSelect.value = 'cppworker-gpu-bundled-agent';
        elements.layoutDonorList.querySelectorAll = function () {
            return [{ checked: true, getAttribute: function () { return 'cppworker-cyborg-bundled-agent'; } }];
        };
        return ML.applySelection().then(function () {
            assert.ok(roleCalls.indexOf('cppworker-gpu-bundled-agent=main') >= 0, 'не отправлен main: ' + roleCalls.join(','));
            assert.ok(roleCalls.indexOf('cppworker-cyborg-bundled-agent=donor') >= 0, 'не отправлен donor: ' + roleCalls.join(','));
            assert.ok(roleCalls.every(function (c) { return c.indexOf('imageworker') === -1; }),
                'image-бэкенду роль не отправляем: ' + roleCalls.join(','));
        });
    });

    await check('R118.4: «Только локально» переводит все llama.cpp-бэкенды в standalone', function () {
        roleCalls.length = 0;
        backendsPayload = [MAIN, DONOR, IMG];
        ML._state.backends = backendsPayload;
        return ML.setAllLocal().then(function () {
            assert.deepStrictEqual(roleCalls.slice().sort(), [
                'cppworker-cyborg-bundled-agent=standalone',
                'cppworker-gpu-bundled-agent=standalone'
            ], 'не все бэкенды переведены локально: ' + roleCalls.join(','));
        });
    });

    await check('R118.4: без главного панель честно говорит, что раскладки нет', function () {
        backendsPayload = [{ id: 'a', backendType: 'llama_cpp', role: 'standalone' }];
        ML._state.backends = backendsPayload;
        ML.renderPanel();
        assert.ok(/Главный не выбран/.test(elements.layoutStatus.innerHTML),
            'нет пояснения про отсутствие главного: ' + elements.layoutStatus.innerHTML);
    });

    console.log('\nOK: ' + checks + ' checks passed');
})().catch(function (e) {
    console.error('\nFAIL: ' + (e && e.message ? e.message : e));
    process.exit(1);
});
