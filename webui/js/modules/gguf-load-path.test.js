// gguf-load-path.test.js — R66d (2026-09-23) регресс на путь «Загрузить модель».
//
// Run: node webui/js/modules/gguf-load-path.test.js
//
// ЧТО ПРОВЕРЯЕТСЯ (жалоба: «через форму WebUI не работает ни загрузка модели на
// llama.cpp-бэкенд, ни смена настроек модели — для preload или для change»):
//
//   1. buildLoadOptions() действительно превращает настройки из формы Settings
//      (state.loadOptions) в поля balancer.ModelOpRequest.
//      БЫЛО: loadOnSelectedBackend вообще не смотрел в state.loadOptions, поэтому
//      ctxSize (и остальные настройки) не доходили до cppworker — модель
//      грузилась с дефолтом cppworker (ctxSize=2048), хотя в форме стояло 32768.
//
//   2. loadOnSelectedBackend вызывает GgufApi.manageModel(backendId, 'load',
//      modelName, options) — то есть идёт через балансер и передаёт ИМЯ МОДЕЛИ.
//      БЫЛО: api.loadModel(handle, model.name, {path}) при сигнатуре
//      loadModel(modelName, options) → в cppworker уходило name=<backendId>,
//      ctxSize=2048, а третий аргумент игнорировался.
//
//   3. Параметры загрузки передаются целиком (contextSize/gpuLayers/batchSize/
//      flashAttn/useMmap/kvCacheType), а не только path.

const assert = require('assert');

// Минимальное окружение браузера: IIFE пишет в window.GgufModule.
global.window = global;
window.GgufModule = {
    state: {
        selectedBackendId: 'cppworker-gpu-bundled-agent',
        registeredBackends: [
            { id: 'cppworker-gpu-bundled-agent', host: 'cppworker-gpu', cppWorkerPort: 18092 },
        ],
        loadingModels: {},
        localModels: [],
        _loadOptionsCustom: {},
        loadOptions: {
            gpuLayers: -1,
            ctxSize: 32768,
            batchSize: 1024,
            flashAttn: true,
            numa: false,
            useMmap: false,
            tensorSplit: null,
            autoGpuDistribution: true,
            strategy: 'vram-ratio',
        },
    },
    showToast: function () {},
    _: function (k, d) { return d || k; },
};

// Захватываем вызовы, которые уходят в API.
const calls = { manageModel: null, loadModel: null, toasts: [] };
global.GgufApi = {
    manageModel: function (backendId, operation, modelName, options) {
        calls.manageModel = { backendId: backendId, operation: operation, modelName: modelName, options: options };
        return Promise.resolve({ success: true, message: 'loaded' });
    },
    loadModel: function (modelName, options) {
        calls.loadModel = { modelName: modelName, options: options };
        return Promise.resolve({ status: 'ok' });
    },
};

require('./gguf-renderer-actions.js');
const M = window.GgufModule;
M.showToast = function (msg, kind) { calls.toasts.push({ msg: msg, kind: kind }); };

// --- 1. buildLoadOptions: настройки формы → параметры balancer API -----------
const opts = M.buildLoadOptions();

assert.strictEqual(opts.contextSize, 32768,
    'ctxSize из формы должен уходить как contextSize (иначе «не меняется контекстное окно»); got ' + JSON.stringify(opts));
assert.strictEqual(opts.gpuLayers, -1, 'gpuLayers должен передаваться');
assert.strictEqual(opts.batchSize, 1024, 'batchSize должен передаваться (HTTP-слой раньше его терял)');
assert.strictEqual(opts.flashAttn, 1, 'flashAttn: bool из формы → int 1 для cppworker');
assert.strictEqual(opts.useMmap, false, 'useMmap должен передаваться');
assert.strictEqual(opts.kvCacheType, undefined, 'пустой kvCacheType не передаём (cppworker применит дефолт)');

// flashAttn=false → 0 (явное выключение, не «не задано»)
M.state.loadOptions.flashAttn = false;
assert.strictEqual(M.buildLoadOptions().flashAttn, 0, 'flashAttn=false должен стать 0, а не отсутствовать');

// ctxSize=0/NaN не должен превращаться в «contextSize: 0» (сброс контекста)
M.state.loadOptions.ctxSize = 0;
assert.strictEqual(M.buildLoadOptions().contextSize, undefined, 'ctxSize=0 не передаём');
M.state.loadOptions.ctxSize = 32768;
M.state.loadOptions.flashAttn = true;

// --- 2/3. loadOnSelectedBackend: балансер + имя модели + все параметры -------
const done = (function () {
    M.loadOnSelectedBackend({ name: 'gemma-4-E4B-it-Q4_K_M.gguf', path: '/app/models/gemma-4-E4B-it-Q4_K_M.gguf' });
    return true;
})();
assert.ok(done, 'loadOnSelectedBackend должен выполняться синхронно до первого await');

assert.ok(calls.manageModel, 'загрузка должна идти через GgufApi.manageModel (балансер), а не напрямую в cppworker');
assert.strictEqual(calls.manageModel.operation, 'load', 'operation должен быть load');
assert.strictEqual(calls.manageModel.backendId, 'cppworker-gpu-bundled-agent', 'backendId — выбранный бэкенд');
assert.strictEqual(calls.manageModel.modelName, 'gemma-4-E4B-it-Q4_K_M.gguf',
    'в API должно уходить ИМЯ МОДЕЛИ, а не id бэкенда (это была основная ошибка)');
assert.strictEqual(calls.manageModel.options.contextSize, 32768, 'ctxSize должен доехать до balancer API');
assert.strictEqual(calls.manageModel.options.batchSize, 1024, 'batchSize должен доехать до balancer API');
assert.strictEqual(calls.manageModel.options.flashAttn, 1, 'flashAttn должен доехать до balancer API');
assert.strictEqual(calls.manageModel.options.useMmap, false, 'useMmap должен доехать до balancer API');
assert.strictEqual(calls.manageModel.options.gpuLayers, -1, 'gpuLayers должен доехать до balancer API');

assert.strictEqual(calls.loadModel, null, 'прямой путь в cppworker (loadModel) не должен использоваться, когда есть manageModel');

// --- 4. Ошибка от балансера показывается пользователю ------------------------
global.GgufApi.manageModel = function () {
    return Promise.resolve({ success: false, error: 'auto-load failed: model not found' });
};
M.loadOnSelectedBackend({ name: 'missing.gguf' });
setTimeout(function () {
    const failed = calls.toasts.some(function (t) {
        return t.kind === 'error' && /auto-load failed/.test(t.msg);
    });
    assert.ok(failed, 'ошибка загрузки должна попадать в UI как есть; toasts=' + JSON.stringify(calls.toasts));
    console.log('gguf-load-path.test.js: OK');
}, 0);
