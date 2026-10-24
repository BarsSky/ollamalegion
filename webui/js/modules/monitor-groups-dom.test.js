// monitor-groups-dom.test.js — R104 (S6 плана 2026-10-07): карточка логических
// бэкендов (объединённых узлов) на странице «Монитор».
//
// Запуск: node webui/js/modules/monitor-groups-dom.test.js
//
// Что проверяем:
//   1. Карточка ПОДКЛЮЧЕНА: контейнер и тег script есть в monitor.html, а рендер
//      размещения в ui-renderer.js вызывает renderFromPlacement (иначе карточка не
//      появится, и это не видно ни в одном чистом тесте).
//   2. Отрисовка идёт из данных, которые монитор УЖЕ получил, без своих запросов:
//      монитор опрашивает /api/v1/placement каждые ~2 с, и отдельный запрос на
//      каждый тик удваивал бы трафик.
//   3. Что показывается: модель, стратегия, роли, VRAM, слоты, загруженное,
//      состояние и причина деградации.
//   4. Честность: скорость «н/д», если измерений не было (а не 0 tok/s); трафик не
//      приклеивается к маске группы (числа чужой модели хуже, чем их отсутствие).

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

let passed = 0;
const failures = [];
function check(name, fn) {
    try { fn(); passed++; console.log('  \u2713 ' + name); }
    catch (e) { failures.push(name + ': ' + (e && e.message)); console.error('  \u2717 ' + name + ' - ' + (e && e.message)); }
}
async function checkAsync(name, fn) {
    try { await fn(); passed++; console.log('  \u2713 ' + name); }
    catch (e) { failures.push(name + ': ' + (e && e.message)); console.error('  \u2717 ' + name + ' - ' + (e && e.message)); }
}

global.window = global;
const panel = { innerHTML: '' };
const domListeners = {};
global.document = {
    readyState: 'loading',
    addEventListener: function (name, fn) { domListeners[name] = fn; },
    getElementById: function (id) { return id === 'monitorGroupsPanel' ? panel : null; },
    querySelectorAll: function () { return []; },
};
const store = {};
global.localStorage = {
    getItem: function (k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem: function (k, v) { store[k] = String(v); },
    removeItem: function (k) { delete store[k]; },
};

require('../i18n/ru.js');
require('../i18n/en.js');
const RU = global.I18N_RU;
const EN = global.I18N_EN;

global.I18N = {
    getLang: function () { return 'ru'; },
    t: function (key) { return RU[key] === undefined ? key : RU[key]; },
};
global.Api = { getAuthHeaders: function () { return { 'Content-Type': 'application/json' }; } };
global.WEBUI_CONFIG = { API_BASE: 'http://balancer.test:18081' };

const calls = [];
let routes = {};
global.fetch = function (url, opts) {
    const method = (opts && opts.method) || 'GET';
    // R163: заголовки тоже запоминаем — иначе 401 из-за отсутствия X-API-Token не поймать.
    calls.push({ method: method, url: url, headers: (opts && opts.headers) || {} });
    const key = method + ' ' + url.replace('http://balancer.test:18081', '');
    const handler = routes[key];
    if (!handler) {
        return Promise.resolve({ ok: false, status: 404, text: function () { return Promise.resolve('{"error":"no route"}'); } });
    }
    const res = typeof handler === 'function' ? handler() : handler;
    return Promise.resolve({
        ok: res.status >= 200 && res.status < 300,
        status: res.status,
        text: function () { return Promise.resolve(JSON.stringify(res.body)); },
    });
};

const MODULE_SRC = fs.readFileSync(path.join(__dirname, 'monitor-groups.js'), 'utf8');
const MONITOR_HTML = fs.readFileSync(path.join(__dirname, '..', '..', 'monitor.html'), 'utf8');
const UI_RENDERER_SRC = fs.readFileSync(path.join(__dirname, '..', 'monitor', 'ui-renderer.js'), 'utf8');

require('./monitor-groups.js');
const MG = global.MonitorGroups;

// --- 1. подключение -------------------------------------------------------

check('контейнер #monitorGroupsPanel объявлен в monitor.html', function () {
    assert.ok(MONITOR_HTML.indexOf('id="monitorGroupsPanel"') >= 0, 'карточке нужен контейнер');
});

check('тег script с модулем есть в monitor.html', function () {
    assert.ok(/js\/modules\/monitor-groups\.js\?v=/.test(MONITOR_HTML), 'модуль должен быть подключён');
});

check('рендер размещения вызывает карточку групп', function () {
    assert.ok(UI_RENDERER_SRC.indexOf('MonitorGroups.renderFromPlacement') >= 0,
        'без вызова карточка не отрисуется на мониторе');
});

check('все i18n-ключи карточки есть в ru.js и en.js', function () {
    const re = /T\('([^']+)'/g;
    const keys = new Set();
    let m;
    while ((m = re.exec(MODULE_SRC)) !== null) {
        // Ключи-ПРЕФИКСЫ (`plan.strategy.` + переменная) пропускаем: полного
        // ключа в исходнике нет по определению, как и в Go-линте i18n.
        if (!/\.$/.test(m[1])) { keys.add(m[1]); }
    }
    keys.add('plan.strategy.layer');
    keys.add('plan.strategy.row');
    keys.add('merge.role.master');
    keys.add('merge.role.worker');
    const missingRU = [];
    const missingEN = [];
    keys.forEach(function (k) {
        if (RU[k] === undefined) { missingRU.push(k); }
        if (EN[k] === undefined) { missingEN.push(k); }
    });
    assert.deepStrictEqual(missingRU, [], 'нет переводов ru: ' + missingRU.join(', '));
    assert.deepStrictEqual(missingEN, [], 'нет переводов en: ' + missingEN.join(', '));
});

// --- 2. отрисовка из готовых данных ---------------------------------------

const placementPayload = {
    enabled: true,
    groups: [
        {
            id: 'group-qwen3-14b', model: 'qwen3-14b', strategy: 'layer',
            masterBackendId: 'node-a', usable: true, healthy: true, degraded: false,
            slots: 1, vramFreeMB: 12288, vramTotalMB: 16384, vramKnownMembers: 2,
            tokensKnown: true, tokensPerSecond: 18.4,
            loadedModels: ['gemma-3-4b', 'qwen3-14b'],
            members: [
                { backendId: 'node-a', role: 'master', healthy: true, registered: true, vramKnown: true, vramFreeMB: 6144, vramTotalMB: 8192, loadedModels: ['qwen3-14b'] },
                { backendId: 'node-b', role: 'worker', rpcEndpoint: '192.0.2.11:50052', healthy: true, registered: true, vramKnown: true, vramFreeMB: 6144, vramTotalMB: 8192, loadedModels: ['gemma-3-4b'] },
            ],
        },
        {
            id: 'group-broken', model: 'm-broken', strategy: 'row',
            usable: false, healthy: true, degraded: true,
            degradeReason: 'узел node-c (worker) не зарегистрирован в балансере',
            slots: 1, vramFreeMB: 2048, vramTotalMB: 8192, vramKnownMembers: 1,
            tokensKnown: false, tokensPerSecond: 0,
            members: [
                { backendId: 'node-c', role: 'worker', healthy: false, registered: false, vramKnown: false, problem: 'не зарегистрирован' },
            ],
        },
    ],
};

(async function run() {
    await checkAsync('renderFromPlacement: группы рисуются БЕЗ своих запросов', async function () {
        MG.reset();
        calls.length = 0;
        MG.renderFromPlacement(placementPayload);
        assert.strictEqual(calls.length, 0,
            'карточка должна рисоваться из данных монитора, а не ходить в балансер на каждом тике');
        assert.ok(panel.innerHTML.indexOf('group-qwen3-14b') >= 0, 'id группы');
        assert.ok(panel.innerHTML.indexOf('qwen3-14b') >= 0, 'модель');
        assert.ok(panel.innerHTML.indexOf('layer') >= 0, 'стратегия');
        assert.ok(panel.innerHTML.indexOf('node-a') >= 0 && panel.innerHTML.indexOf('node-b') >= 0, 'узлы');
        assert.ok(panel.innerHTML.indexOf('мастер') >= 0, 'роль мастера переведена');
        assert.ok(panel.innerHTML.indexOf('воркер') >= 0, 'роль воркера переведена');
        assert.ok(panel.innerHTML.indexOf('6.0 GB') >= 0, 'VRAM узла');
        assert.ok(panel.innerHTML.indexOf('12.0 GB') >= 0, 'VRAM по группе');
        assert.ok(panel.innerHTML.indexOf('192.0.2.11:50052') >= 0, 'RPC-эндпоинт воркера');
        assert.ok(panel.innerHTML.indexOf('gemma-3-4b') >= 0, 'загруженные модели узла');
        assert.ok(panel.innerHTML.indexOf('18.4 tok/s') >= 0, 'измеренная скорость');
        assert.ok(panel.innerHTML.indexOf('готова') >= 0, 'состояние группы');
    });

    check('деградация показана с причиной и без выдуманных чисел', function () {
        assert.ok(panel.innerHTML.indexOf('не зарегистрирован в балансере') >= 0, 'причина деградации');
        assert.ok(panel.innerHTML.indexOf('н/д') >= 0,
            'без измерений скорость должна быть «н/д», а не 0 tok/s');
        assert.ok(panel.innerHTML.indexOf('деградировала') >= 0, 'состояние degraded');
    });

    check('трафик не приклеивается к маске группы', function () {
        // Числа есть только для конкретной модели; у группы с маской их быть не
        // должно: показать чужие числа хуже, чем не показать ничего.
        MG.reset();
        MG.state.trafficByModel = MG.indexTraffic({
            plans: [{ model: 'qwen3-14b', traffic: { layer: { known: true, tokenOverheadPercent: 0.22, prefillMs: 37.4 } } }],
        });
        MG.renderFromPlacement(placementPayload);
        assert.ok(panel.innerHTML.indexOf('0.2 %') >= 0, 'трафик точного совпадения показывается');

        MG.reset();
        MG.state.trafficByModel = MG.indexTraffic({
            plans: [{ model: 'другая-модель', traffic: { layer: { known: true, tokenOverheadPercent: 9.9, prefillMs: 100 } } }],
        });
        MG.renderFromPlacement({ groups: [placementPayload.groups[0]] });
        assert.ok(panel.innerHTML.indexOf('9.9') < 0, 'чужие числа показывать нельзя');
    });

    check('пустой список групп — честная подсказка', function () {
        MG.reset();
        MG.renderFromPlacement({ groups: [] });
        assert.ok(panel.innerHTML.indexOf('Логических бэкендов нет') >= 0,
            'пустая карточка должна объяснять, что делать');
    });

    await checkAsync('refresh(): GET состояния + POST плана', async function () {
        routes = {
            'GET /api/v1/placement': { status: 200, body: placementPayload },
            'POST /api/v1/placement/plan': {
                status: 200,
                body: { plans: [{ model: 'qwen3-14b', traffic: { layer: { known: true, tokenOverheadPercent: 0.22, prefillMs: 37.4 } } }] },
            },
        };
        MG.reset();
        calls.length = 0;
        const groups = await MG.refresh();
        assert.strictEqual(groups.length, 2, 'обе группы должны попасть в состояние');
        assert.strictEqual(calls.length, 2, 'состояние + план, не больше');
        assert.ok(panel.innerHTML.indexOf('0.2 %') >= 0, 'трафик из плана должен появиться');
    });

    await checkAsync('ошибка загрузки показывается, а не остаётся пустой карточкой', async function () {
        routes = { 'GET /api/v1/placement': { status: 500, body: { error: 'boom' } } };
        MG.reset();
        let failed = false;
        try { await MG.refresh(); } catch (e) { failed = true; }
        assert.ok(failed, 'ошибка должна дойти до вызывающего');
        assert.ok(panel.innerHTML.indexOf('Не удалось получить состояние групп') >= 0);
    });

    check('autoMount регистрируется на DOMContentLoaded', function () {
        assert.strictEqual(typeof domListeners['DOMContentLoaded'], 'function',
            'карточка должна сама монтироваться на странице монитора');
    });

    check('trafficText: неизвестные числа не выдумываются', function () {
        assert.strictEqual(MG.trafficText(null), '');
        assert.strictEqual(MG.trafficText({ layer: { known: false } }), '');
    });

    // ── R163 (2026-10-17): авторизация запросов карточки групп ─────────────────
    //
    // ЖИВОЙ ДЕФЕКТ: на дашборде POST /api/v1/placement/plan уходил БЕЗ X-API-Token
    // и получал 401. Причина — порядок подключения: monitor.html грузит
    // monitor-groups.js ДО js/monitor/api.js, а общего window.Api на странице
    // монитора нет вовсе (своя реализация живёт в js/monitor/api.js).
    //
    // КОНТРФАКТУАЛЬНОСТЬ: на старом коде первый тест падал — заголовка не было.

    await checkAsync('R163: без window.Api (страница монитора) токен всё равно уходит', async function () {
        const savedApi = global.Api;
        const savedCfg = global.WEBUI_CONFIG;
        delete global.Api;
        global.WEBUI_CONFIG = { API_BASE: 'http://balancer.test:18081', API_TOKEN: 'tok-r163' };
        try {
            routes = {
                'GET /api/v1/placement': { status: 200, body: placementPayload },
                'POST /api/v1/placement/plan': { status: 200, body: { plans: [] } },
            };
            MG.reset();
            calls.length = 0;
            await MG.refresh();
            const post = calls.filter(function (c) { return c.method === 'POST'; })[0];
            assert.ok(post, 'POST плана обязан уйти');
            assert.strictEqual(post.headers['X-API-Token'], 'tok-r163',
                'без токена балансер ответит 401 — это и был живой дефект');
        } finally {
            global.Api = savedApi;
            global.WEBUI_CONFIG = savedCfg;
        }
    });

    await checkAsync('R163: с window.Api токен берётся из него (без регрессии)', async function () {
        const savedApi = global.Api;
        global.Api = {
            getAuthHeaders: function () {
                return { 'Content-Type': 'application/json', 'X-API-Token': 'tok-from-api' };
            },
        };
        try {
            routes = {
                'GET /api/v1/placement': { status: 200, body: placementPayload },
                'POST /api/v1/placement/plan': { status: 200, body: { plans: [] } },
            };
            MG.reset();
            calls.length = 0;
            await MG.refresh();
            const post = calls.filter(function (c) { return c.method === 'POST'; })[0];
            assert.strictEqual(post.headers['X-API-Token'], 'tok-from-api',
                'основное приложение отдаёт токен своим построителем');
        } finally {
            global.Api = savedApi;
        }
    });

    console.log('\n' + passed + ' passed, ' + failures.length + ' failed');
    if (failures.length) {
        failures.forEach(function (f) { console.error('  - ' + f); });
        process.exit(1);
    }
})();
