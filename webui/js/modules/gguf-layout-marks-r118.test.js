// gguf-layout-marks-r118.test.js — R118 (2026-10-14).
//
// Запуск: node webui/js/modules/gguf-layout-marks-r118.test.js
//
// ЖАЛОБА: «не хватает пометок в webui, что осуществлена раскладка на донора и по
// какой модели и по каким слоям, а также замер пропускной способности канала».
//
// Что фиксирует тест:
//   1. в строке загруженной модели появляется чип «раскладка» с узлом-донором,
//      диапазонами слоёв по устройствам и числом слоёв на CPU;
//   2. появляется чип «канал» с замером (Мбит/с и МБ/с) и источником замера;
//   3. без данных раскладки чипов НЕТ (ничего не выдумываем);
//   4. расчёт помечен как расчёт (в title есть предупреждение и указание, где
//      точный список слоёв).
//
// Живые значения взяты с прогона мастер+донор: 42 слоя, gpu_layers=10,
// CUDA0 33–38 (6), RPC0 39–42 (4), CPU 33, канал 889 Мбит/с (замер 14.10.2026).

'use strict';

const assert = require('assert');

global.window = global;
global.Utils = { escapeHtml: function (s) { return String(s === null || s === undefined ? '' : s); } };
global.I18N = { t: function (key) { return key; } };

global.GgufModule = {
    state: {
        selectedBackendId: 'bk-1',
        registeredBackends: [{ id: 'bk-1', backendType: 'llama_cpp' }],
        detailPane: 'models',
        localModels: [],
        loadedModels: [],
        runtimeModels: {},
        rpcLayout: null,
        workerInfo: null,
        gpuInfo: null,
        diskInventory: {}
    },
    _: function (key) { return key; },
    stripGGUF: function (n) { return n; },
    formatFileSize: function () { return '-'; },
    formatVramMB: function (n) { return String(n); },
    showToast: function () {},
    refreshDetailPane: function () {},
    refreshDetailPanel: function () {}
};
global.GgufApi = {
    getRuntimeConfigViaBackend: function () { return Promise.resolve({ loaded_models: [] }); },
    listLocalModelsViaBackend: function () { return Promise.resolve({ files: [] }); },
    requestViaBackend: function () { return Promise.resolve({ files: [] }); }
};

require('./gguf-renderer-refresh.js');
require('./gguf-renderer-detail-render.js');
const M = global.GgufModule;
assert.ok(typeof M.buildLayoutFlags === 'function', 'buildLayoutFlags должен быть опубликован');

let checks = 0;
function check(name, fn) { fn(); checks++; console.log('  ✓ ' + name); }

// Живая раскладка: узел-донор + слои по устройствам + замер канала.
const LIVE_LAYOUT = {
    active: true,
    nodes: ['192.0.2.10:50052'],
    remote_nodes: 1,
    vram_local_free_mb: 7097,
    vram_remote_free_mb: 7033,
    vram_layout_free_mb: 14130,
    link_mbps: 889,
    link_measured_at: '2026-10-14',
    link_source: 'scripts/measure-network.ps1'
};
const LIVE_MODEL = {
    name: 'gemma-4-E4B-it-Q4_K_M',
    device_layers: {
        total_layers: 42,
        gpu_layers: 10,
        cpu_layers: 33,
        estimated: true,
        note: 'расчёт по n_layers/gpu_layers/tensor_split; точный список — в логе cppworker',
        devices: [
            { device: 'CUDA0', kind: 'local', count: 6, from: 33, to: 38 },
            { device: 'RPC0', kind: 'rpc', node: '192.0.2.10:50052', count: 4, from: 39, to: 42 }
        ]
    }
};

check('R118: чип «раскладка» показывает донора, слои по устройствам и CPU', function () {
    const flags = M.buildLayoutFlags(LIVE_MODEL, LIVE_LAYOUT);
    const lay = flags.filter(function (f) { return f.label === 'раскладка'; })[0];
    assert.ok(lay, 'нет чипа «раскладка»: ' + JSON.stringify(flags));
    assert.ok(/донор 192\.0\.2\.10:50052/.test(lay.value), 'нет узла-донора: ' + lay.value);
    assert.ok(/CUDA0 33–38 \(6\)/.test(lay.value), 'нет диапазона локальных слоёв: ' + lay.value);
    assert.ok(/RPC0 39–42 \(4\) → 192\.0\.2\.10:50052/.test(lay.value),
        'нет диапазона слоёв на доноре: ' + lay.value);
    assert.ok(/CPU \(33\)/.test(lay.value), 'нет слоёв на CPU: ' + lay.value);
    assert.strictEqual(lay.tone, 'ok', 'активная раскладка должна быть положительным тоном');
});

check('R118: чип «канал» показывает замер в Мбит/с и МБ/с с источником', function () {
    const flags = M.buildLayoutFlags(LIVE_MODEL, LIVE_LAYOUT);
    const link = flags.filter(function (f) { return f.label === 'канал'; })[0];
    assert.ok(link, 'нет чипа «канал»: ' + JSON.stringify(flags));
    assert.strictEqual(link.value, '889 Мбит/с (111.1 МБ/с)', 'значение замера: ' + link.value);
    assert.ok(/measure-network\.ps1/.test(link.title), 'в подсказке нет источника замера: ' + link.title);
    assert.ok(/2026-10-14/.test(link.title), 'в подсказке нет даты замера: ' + link.title);
});

check('R118: расчёт помечен как расчёт (точный список — в логе cppworker)', function () {
    const flags = M.buildLayoutFlags(LIVE_MODEL, LIVE_LAYOUT);
    const lay = flags.filter(function (f) { return f.label === 'раскладка'; })[0];
    assert.ok(/расчёт/.test(lay.title), 'в подсказке нет пометки «расчёт»: ' + lay.title);
    assert.ok(/cppworker/.test(lay.title), 'нет указания, где точный список слоёв: ' + lay.title);
    assert.ok(/42/.test(lay.title) && /на GPU: 10/.test(lay.title),
        'в подсказке нет чисел слоёв: ' + lay.title);
});

check('R118: без данных раскладки чипов нет — ничего не выдумываем', function () {
    assert.deepStrictEqual(M.buildLayoutFlags({ name: 'm' }, null), []);
    assert.deepStrictEqual(M.buildLayoutFlags({ name: 'm', device_layers: null }, { active: false }), []);
    // Раскладка активна, но замер канала не задан — чипа «канал» быть не должно.
    const noLink = Object.assign({}, LIVE_LAYOUT, { link_mbps: 0 });
    const labels = M.buildLayoutFlags(LIVE_MODEL, noLink).map(function (f) { return f.label; });
    assert.ok(labels.indexOf('канал') === -1, 'чип «канал» без замера: ' + labels.join(','));
});

check('R118: пометки попадают в строку флагов загруженной модели', function () {
    M.state.rpcLayout = LIVE_LAYOUT;
    const flags = M.buildLoadedFlags(LIVE_MODEL, {}, LIVE_LAYOUT);
    const labels = flags.map(function (f) { return f.label; });
    assert.ok(labels.indexOf('раскладка') !== -1, 'в строке модели нет чипа раскладки: ' + labels.join(','));
    assert.ok(labels.indexOf('канал') !== -1, 'в строке модели нет чипа канала: ' + labels.join(','));
    // И при отсутствии раскладки строка остаётся прежней.
    M.state.rpcLayout = null;
    const plain = M.buildLoadedFlags({ name: 'm' }, {});
    assert.ok(plain.every(function (f) { return f.label !== 'раскладка' && f.label !== 'канал'; }),
        'лишние чипы раскладки без данных: ' + JSON.stringify(plain));
});

check('R118: вывод плана совпадает с логом llama.cpp на трёх живых прогонах', function () {
    assert.ok(typeof M.deriveLayerPlanJS === 'function', 'deriveLayerPlanJS должен быть опубликован');
    const donor = ['192.0.2.10:50052'];
    const cases = [
        { name: 'все слои, split 0.537/0.463', gpu: 42, split: [0.53704435, 0.46295568],
          want: [[6, 23, 1, 23], [6, 19, 24, 42]], cpu: 1 },
        { name: 'gpu=10, split 0.537/0.463', gpu: 10, split: [0.53704435, 0.46295568],
          want: [[6, 6, 33, 38], [6, 4, 39, 42]], cpu: 33 },
        { name: 'gpu=10, split 0.605/0.395 (живой прогон 7/3)', gpu: 10, split: [0.6054849, 0.39451507],
          want: [[6, 7, 33, 39], [6, 3, 40, 42]], cpu: 33 }
    ];
    cases.forEach(function (c) {
        const plan = M.deriveLayerPlanJS(42, c.gpu, c.split, 1, donor);
        assert.ok(plan, c.name + ': план не построен');
        assert.strictEqual(plan.cpu_layers, c.cpu, c.name + ': CPU=' + plan.cpu_layers + ' want ' + c.cpu);
        assert.strictEqual(plan.devices.length, 2, c.name + ': устройств=' + plan.devices.length);
        c.want.forEach(function (w, i) {
            const d = plan.devices[i];
            assert.strictEqual(d.count, w[1], c.name + ': count[' + i + ']=' + d.count + ' want ' + w[1]);
            assert.strictEqual(d.from, w[2], c.name + ': from[' + i + ']=' + d.from + ' want ' + w[2]);
            assert.strictEqual(d.to, w[3], c.name + ': to[' + i + ']=' + d.to + ' want ' + w[3]);
            if (i === 1) assert.strictEqual(d.node, donor[0], c.name + ': нет узла у RPC-устройства');
        });
        const sum = plan.devices.reduce(function (a, d) { return a + d.count; }, 0);
        assert.strictEqual(sum, c.gpu, c.name + ': сумма по устройствам=' + sum + ' want ' + c.gpu);
    });
    // Без данных — плана нет (ничего не выдумываем).
    assert.strictEqual(M.deriveLayerPlanJS(0, 10, [], 1, donor), null);
});

check('R118.2: деградация раскладки видна оператору отдельным чипом', function () {
    // Балансер выключил раскладку, потому что узел не отвечает (R118.2), и модель
    // загрузилась локально. Оператор должен видеть причину, а не гадать.
    const degraded = Object.assign({}, LIVE_LAYOUT, {
        active: false,
        degraded: true,
        degrade_reason: 'узлы раскладки недоступны (192.0.2.10:50052): раскладка выключена, загрузка идёт локально'
    });
    const flags = M.buildLayoutFlags(LIVE_MODEL, degraded);
    const warn = flags.filter(function (f) { return f.tone === 'warn'; })[0];
    assert.ok(warn, 'нет предупреждающего чипа о деградации: ' + JSON.stringify(flags));
    assert.ok(/192\.0\.2\.10:50052/.test(warn.value), 'в чипе нет недоступного узла: ' + warn.value);
    assert.ok(/локально/.test(warn.value), 'в чипе нет пояснения про локальную загрузку: ' + warn.value);
    assert.ok(/вернётся — раскладка включится сама/.test(warn.title),
        'в подсказке нет autorestore раскладки: ' + warn.title);

    // Полностью здоровая раскладка предупреждения не даёт.
    const okFlags = M.buildLayoutFlags(LIVE_MODEL, LIVE_LAYOUT);
    assert.strictEqual(okFlags.filter(function (f) { return f.tone === 'warn'; }).length, 0,
        'здоровая раскладка не должна давать предупреждений');
});

check('R118.15: оба числа о канале показываются раздельно (оператор и автозамер)', function () {
    // Живой факт со стенда: замер оператора хост↔хост 889 Мбит/с, автозамер
    // балансера через контейнерную сеть 127 Мбит/с. Это РАЗНЫЕ числа о разных
    // путях — оба должны быть видны, с подписями источников.
    const layout = Object.assign({}, LIVE_LAYOUT, {
        link_auto_mbps: 127,
        link_auto_at: '2026-10-14T23:52:01Z',
        link_auto_node: '192.0.2.10:18092'
    });
    const flags = M.buildLayoutFlags(LIVE_MODEL, layout).filter(function (f) { return f.label === 'канал'; });
    assert.strictEqual(flags.length, 2, 'ожидались два чипа «канал», получено ' + flags.length);
    assert.ok(/889 Мбит\/с/.test(flags[0].value), 'нет замера оператора: ' + flags[0].value);
    assert.ok(/127 Мбит\/с \(авто\)/.test(flags[1].value), 'нет автозамера: ' + flags[1].value);
    assert.ok(/контейнерную сеть/.test(flags[1].title),
        'в подсказке автозамера не объяснён путь через контейнерную сеть: ' + flags[1].title);
    assert.ok(/192\.0\.2\.10:18092/.test(flags[1].title), 'нет узла автозамера: ' + flags[1].title);

    // Без автозамера — прежнее поведение: один чип оператора.
    const onlyOperator = M.buildLayoutFlags(LIVE_MODEL, LIVE_LAYOUT)
        .filter(function (f) { return f.label === 'канал'; });
    assert.strictEqual(onlyOperator.length, 1, 'лишний чип канала без автозамера');
});
console.log('\nOK: ' + checks + ' checks passed');
