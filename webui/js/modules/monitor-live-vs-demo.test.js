// monitor-live-vs-demo.test.js — R238b (2026-10-25): где конвейер «фактовый», а где живой.
//
// ЖИВОЙ ДЕФЕКТ. После R238 монитор брал события конвейера так:
//   var recentRequests = data.cluster.recentRequests || [];
// В живом ответе балансера таких полей нет, получался пустой массив `[]`, а он
// в JS truthy — значит `demoLifecycle` включался и в живом режиме. Следствия:
// случайные частицы топологии и discovery отключались (они проверяют
// demoLifecycle), а частиц ответа не появлялось (событий нет). Живой монитор
// оставался почти без анимации.
//
// Контракт MonitorApp.lifecyclePayload(cluster), который здесь фиксируется:
//   * живой payload (нет demo и нет массивов) -> null-ы, demoLifecycle=false;
//   * демо-payload (demo:true) -> массивы (даже пустые), demoLifecycle=true;
//   * payload с полями recentRequests/recentResponses -> тоже событийный режим.
//
// Запуск: node webui/js/modules/monitor-live-vs-demo.test.js

'use strict';

const assert = require('assert');

// Минимальное окружение для state.js (как в topology-layout.test.js).
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
assert.ok(MA && typeof MA.lifecyclePayload === 'function', 'MonitorApp.lifecyclePayload должен существовать');

let checks = 0;
function check(name, fn) { fn(); checks++; console.log('  ok ' + name); }

check('живой payload: событийного режима нет', function () {
    const live = { backends: [{ id: 'cppworker-gpu-bundled-agent' }], rps: 0.3 };
    const p = MA.lifecyclePayload(live);
    assert.strictEqual(p.demo, false);
    assert.strictEqual(p.recentRequests, null);
    assert.strictEqual(p.recentResponses, null);
});

check('живой payload с demo:false — тоже нет', function () {
    const p = MA.lifecyclePayload({ demo: false });
    assert.strictEqual(p.demo, false);
    assert.strictEqual(p.recentRequests, null);
    assert.strictEqual(p.recentResponses, null);
});

check('пустые массивы в живом payload не включают демо-режим', function () {
    // Ровно дефект R238: раньше [] включал demoLifecycle.
    const p = MA.lifecyclePayload({ backends: [], recentRequests: undefined, recentResponses: undefined });
    assert.strictEqual(p.demo, false);
    assert.strictEqual(p.recentRequests, null);
});

check('демо-payload: событийный режим и массивы (даже пустые)', function () {
    const p = MA.lifecyclePayload({ demo: true });
    assert.strictEqual(p.demo, true);
    assert.deepStrictEqual(p.recentRequests, []);
    assert.deepStrictEqual(p.recentResponses, []);
});

check('демо-payload с событиями сохраняет их', function () {
    const req = { requestId: 'req-001', clientName: 'Cline' };
    const resp = { requestId: 'req-001', model: 'qwen' };
    const p = MA.lifecyclePayload({ demo: true, recentRequests: [req], recentResponses: [resp] });
    assert.deepStrictEqual(p.recentRequests, [req]);
    assert.deepStrictEqual(p.recentResponses, [resp]);
});

check('будущий живой payload с массивами событий тоже событийный', function () {
    const p = MA.lifecyclePayload({ recentRequests: [] });
    assert.strictEqual(p.demo, true);
    assert.deepStrictEqual(p.recentRequests, []);
});

console.log('\nOK: ' + checks + ' checks passed');
