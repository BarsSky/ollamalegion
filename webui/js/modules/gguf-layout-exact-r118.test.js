// gguf-layout-exact-r118.test.js — R118.16 (2026-10-14).
//
// Запуск: node webui/js/modules/gguf-layout-exact-r118.test.js
//
// ЖАЛОБА: «не хватает пометок в webui, что осуществлена раскладка на донора и по
// какой модели и по каким слоям». R118 показал эти слои РАСЧЁТОМ; теперь
// cppworker отдаёт точный список — строки лога llama.cpp, которые перехватил мост
// (device_layers_exact). Этот тест фиксирует:
//   1. точный список превращается в план с estimated=false и теми же числами,
//      что в `docker logs` воркера;
//   2. неполный/чужой факт отбрасывается (сумма обязана быть n_layer+1) — тогда
//      показывается расчёт, а не чужая раскладка;
//   3. факт важнее расчёта, и расчётная формула его больше не перекрывает;
//   4. в чипе «раскладка» видно, что это факт из лога, а не оценка.
//
// Живые числа — из лога контейнера на стенде мастер+донор (загрузка
// gemma-4-E4B-it-Q4_K_M, 42 слоя, 43 назначения):
//   layer 0 → CPU, layer 1..23 → CUDA0, layer 24..42 → RPC0[192.0.2.10:50052]

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

let checks = 0;
function check(name, fn) { fn(); checks++; console.log('  ✓ ' + name); }

assert.ok(typeof M.exactLayerPlanJS === 'function', 'exactLayerPlanJS должен быть опубликован');
assert.ok(typeof M.resolveDeviceLayers === 'function', 'resolveDeviceLayers должен быть опубликован');

// Точный список ровно в том виде, в каком его отдаёт cppworker (/api/models и
// /api/v1/cppworker/config/runtime): массив диапазонов + адрес узла у RPC.
const LIVE_EXACT = [
    { device: 'CPU', from: 0, to: 0, count: 1 },
    { device: 'CUDA0', from: 1, to: 23, count: 23 },
    { device: 'RPC0', from: 24, to: 42, count: 19, node: '192.0.2.10:50052' }
];
const LIVE_LAYOUT = {
    active: true,
    nodes: ['192.0.2.10:50052'],
    link_mbps: 889,
    link_measured_at: '2026-10-14',
    link_source: 'scripts/measure-network.ps1'
};

check('R118.16: точный список → план с estimated=false и числами из лога', function () {
    const plan = M.exactLayerPlanJS(LIVE_EXACT, 42);
    assert.ok(plan, 'план не построен');
    assert.strictEqual(plan.estimated, false, 'план из лога обязан быть не-расчётом');
    assert.strictEqual(plan.total_layers, 43, 'total_layers = ' + plan.total_layers);
    assert.strictEqual(plan.gpu_layers, 42, 'gpu_layers = ' + plan.gpu_layers);
    assert.strictEqual(plan.cpu_layers, 1, 'cpu_layers = ' + plan.cpu_layers);
    assert.ok(/точный список из лога llama\.cpp/.test(plan.note), 'нет пометки источника: ' + plan.note);
    assert.strictEqual(plan.devices.length, 2, 'CPU не должен попадать в devices: ' + JSON.stringify(plan.devices));
    assert.deepStrictEqual(plan.devices[0],
        { device: 'CUDA0', kind: 'local', node: '', count: 23, from: 1, to: 23 });
    assert.deepStrictEqual(plan.devices[1],
        { device: 'RPC0', kind: 'rpc', node: '192.0.2.10:50052', count: 19, from: 24, to: 42 });
    // Слои сходятся: GPU-устройства + CPU = total.
    assert.strictEqual(plan.devices[0].count + plan.devices[1].count + plan.cpu_layers, plan.total_layers);
});

check('R118.16: неполный или чужой факт отбрасывается (сумма обязана быть n_layer+1)', function () {
    assert.strictEqual(M.exactLayerPlanJS(null, 42), null);
    assert.strictEqual(M.exactLayerPlanJS([], 42), null);
    assert.strictEqual(M.exactLayerPlanJS(LIVE_EXACT, 0), null, 'без n_layers проверить полноту нельзя');
    // Не хватает слоя — это раскладка ДРУГОЙ модели (или обрезанный ответ).
    assert.strictEqual(M.exactLayerPlanJS(LIVE_EXACT.slice(1), 42), null);
    // Устройство без имени или пустой диапазон — мусор, а не факт.
    assert.strictEqual(M.exactLayerPlanJS([{ from: 0, to: 42, count: 43 }], 42), null);
    assert.strictEqual(M.exactLayerPlanJS([{ device: 'CUDA0', count: 0 }], 42), null);
});

check('R118.16: факт важнее расчёта — расчётная формула его не перекрывает', function () {
    // Модель загружена с gpu_layers=10, но в логе видно, что на GPU ушло 42 слоя
    // (авто-reload поднял окно и переложил слои). Показать надо ЛОГ.
    const state = {
        rpcLayout: LIVE_LAYOUT,
        runtimeModels: {
            'gemma-4-E4B-it-Q4_K_M': {
                name: 'gemma-4-E4B-it-Q4_K_M',
                n_layers: 42, gpu_layers: 10,
                tensor_split: [0.53704435, 0.46295568], gpu_count: 1,
                device_layers_exact: LIVE_EXACT
            },
            // Вторая модель: факта нет — для неё остаётся расчёт (и он помечен).
            'qwen3-4b': { name: 'qwen3-4b', n_layers: 42, gpu_layers: 10, tensor_split: [0.5, 0.5], gpu_count: 1 }
        }
    };
    M.resolveDeviceLayers(state);

    const exact = state.runtimeModels['gemma-4-E4B-it-Q4_K_M'].device_layers;
    assert.strictEqual(exact.estimated, false, 'факт перекрыт расчётом');
    assert.strictEqual(exact.devices[0].from, 1, 'диапазоны взяты не из лога: ' + JSON.stringify(exact.devices));
    assert.strictEqual(exact.cpu_layers, 1, 'CPU не из лога: ' + exact.cpu_layers);

    const derived = state.runtimeModels['qwen3-4b'].device_layers;
    assert.ok(derived, 'для модели без факта расчёт обязан остаться');
    assert.strictEqual(derived.estimated, true, 'расчёт обязан быть помечен estimated=true');
});

check('R118.16: факт показывается даже при ВЫКЛЮЧЕННОЙ раскладке', function () {
    // Раскладка выключена (например, узел недоступен и балансер грузит локально),
    // но точный список из лога всё равно известен — оператор должен его видеть.
    const state = {
        rpcLayout: { active: false, nodes: [] },
        runtimeModels: { m: { name: 'm', n_layers: 42, gpu_layers: 10, device_layers_exact: LIVE_EXACT } }
    };
    M.resolveDeviceLayers(state);
    assert.strictEqual(state.runtimeModels.m.device_layers.estimated, false,
        'без активной раскладки факт потерялся: ' + JSON.stringify(state.runtimeModels.m.device_layers));
});

check('R118.16: чип «раскладка» подписан как факт, а не как расчёт', function () {
    const model = { name: 'gemma-4-E4B-it-Q4_K_M', device_layers: M.exactLayerPlanJS(LIVE_EXACT, 42) };
    const flags = M.buildLayoutFlags(model, LIVE_LAYOUT);
    const lay = flags.filter(function (f) { return f.label === 'раскладка'; })[0];
    assert.ok(lay, 'нет чипа «раскладка»: ' + JSON.stringify(flags));
    assert.ok(/CUDA0 1–23 \(23\)/.test(lay.value), 'нет точного диапазона CUDA0: ' + lay.value);
    assert.ok(/RPC0 24–42 \(19\) → 192\.0\.2\.10:50052/.test(lay.value),
        'нет точного диапазона на доноре: ' + lay.value);
    assert.ok(/CPU \(1\)/.test(lay.value), 'нет слоёв на CPU: ' + lay.value);
    assert.ok(/точный список из лога llama\.cpp/.test(lay.title), 'в подсказке нет источника: ' + lay.title);
    assert.ok(!/расчёт по n_layers/.test(lay.title), 'факт подписан как расчёт: ' + lay.title);
});

console.log('\nOK: ' + checks + ' checks passed');
