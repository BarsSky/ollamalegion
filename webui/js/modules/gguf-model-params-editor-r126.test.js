// gguf-model-params-editor-r126.test.js — R126 (2026-10-16), пункты 5-6 разбора стенда.
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «удобно было бы иметь возможность не только видеть
// загруженную модель и её параметры, но и раскрыть списком параметры, изменить и
// нажать кнопкой применить, чтобы она пошла в перезагрузку… также такой же
// механизм удобен был бы и во вкладке модели на диске — нажать на элемент списка
// (по имени модели) и выбрать ей параметры».
//
// Проверяется:
//   1. редактор рендерится с пояснениями к параметрам и кнопкой применения;
//   2. пустое поле НЕ попадает в тело профиля (иначе «применить» зафиксировало бы
//      пустоту вместо наследования профиль → дефолты → env);
//   3. применение идёт документированным путём: PUT профиля модели, затем
//      существующий apply с перезагрузкой; отсутствие модуля прогресса приводит к
//      честному сообщению, а не к молчаливому «применили»;
//   4. редактор есть и в карточке загруженной модели, и в списке моделей на диске;
//   5. значения загруженной модели берутся из provenance/runtime, а не выдумываются.
//
// Запуск: node webui/js/modules/gguf-model-params-editor-r126.test.js
'use strict';

const assert = require('assert');

global.window = global;
global.Utils = {
    escapeHtml: function (s) {
        return String(s === null || s === undefined ? '' : s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    },
    updateGpuLayersBadge: function () {},
    setGpuLayers: function () {},
};
global.I18N = { t: function (key) { return key; } };

const toasts = [];
global.GgufModule = {
    state: {
        selectedBackendId: 'bk-1',
        registeredBackends: [{ id: 'bk-1', backendType: 'llama_cpp' }],
        detailPane: 'loaded',
        localModels: [],
        loadedModels: [],
        runtimeModels: {},
        rpcLayout: null,
        workerInfo: null,
        gpuInfo: null,
        diskInventory: {},
    },
    _: function (key, fallback) { return fallback || key; },
    stripGGUF: function (n) { return n; },
    formatFileSize: function () { return '-'; },
    formatVramMB: function (n) { return String(n); },
    showToast: function (msg, kind) { toasts.push({ msg: msg, kind: kind }); },
    refreshDetailPane: function () {},
    refreshDetailPanel: function () {},
    currentBackend: function () { return { id: 'bk-1' }; },
};
const apiCalls = [];
global.GgufApi = {
    getRuntimeConfigViaBackend: function () { return Promise.resolve({ loaded_models: [] }); },
    listLocalModelsViaBackend: function () { return Promise.resolve({ files: [] }); },
    requestViaBackend: function (backendId, path, opts) {
        apiCalls.push({ backendId: backendId, path: path, method: (opts && opts.method) || 'GET', body: opts && opts.body });
        return Promise.resolve({ status: 'saved' });
    },
};

// Минимальный DOM: только то, что нужно collectModelParams.
function makeDom(fields) {
    const bySelector = {};
    const root = {
        querySelectorAll: function (sel) {
            if (sel === '.gguf-mp-field') return fields;
            return [];
        },
        getAttribute: function () { return null; },
        querySelector: function () { return null; },
        classList: { add: function () {}, remove: function () {} },
        innerHTML: '',
    };
    bySelector['.gguf-mp-editor[data-model="gemma-4-E4B-it-Q4_K_M"]'] = root;
    global.document = {
        querySelector: function (sel) { return bySelector[sel] || null; },
        getElementById: function () { return null; },
        querySelectorAll: function () { return []; },
    };
    return root;
}

require('./gguf-renderer-detail-render.js');
const M = global.GgufModule;

let failures = 0;
function check(name, cond, detail) {
    if (cond) { console.log('  PASS ' + name); }
    else { failures++; console.log('  FAIL ' + name + (detail ? ' — ' + detail : '')); }
}

console.log('=== R126: редактор параметров модели в карточке ===\n');

check('экспортированы renderModelParamsEditor/collectModelParams/applyModelParams',
    typeof M.renderModelParamsEditor === 'function' &&
    typeof M.collectModelParams === 'function' &&
    typeof M.applyModelParams === 'function');

const html = M.renderModelParamsEditor('gemma-4-E4B-it-Q4_K_M', {
    // R134 (2026-10-16): имя поля — КАК В ПРОФИЛЕ (numGpuLayers). До этого форма
    // читала gpuLayers, а значения приходили как numGpuLayers, и поле «Слои GPU»
    // оставалось пустым даже у загруженной модели — тот самый рассинхрон.
    contextLength: 8192, numGpuLayers: -1, batchSize: 512, parallel: 2, kvCacheType: 'q4_0', flashAttn: true,
}, { source: 'loaded' });

check('редактор содержит поля параметров', ['contextLength', 'numGpuLayers', 'batchSize', 'parallel', 'kvCacheType']
    .every(function (f) { return html.indexOf('data-field="' + f + '"') !== -1; }));
check('значения загруженной модели предзаполнены',
    html.indexOf('value="8192"') !== -1 && html.indexOf('value="-1"') !== -1 && html.indexOf('value="q4_0"') !== -1);
check('R134: форма и loadedParamsFor используют ОДНО имя поля (numGpuLayers)',
    M.loadedParamsFor({ gpuLayers: -1 }, {}).numGpuLayers === -1 &&
    html.indexOf('data-field="numGpuLayers"') !== -1 && html.indexOf('data-field="gpuLayers"') === -1);
check('у параметров есть пояснения', (html.match(/gguf-mp-hint/g) || []).length >= 4);
check('есть кнопка «применить и перезагрузить»', html.indexOf('data-model-params-apply="gemma-4-E4B-it-Q4_K_M"') !== -1);
check('честно сказано, что модель перезагрузится', /перезагрузит|перезагрузк/i.test(html));
check('имя модели экранировано в атрибутах', M.renderModelParamsEditor('a"b<c', {}).indexOf('data-model="a&quot;b&lt;c"') !== -1);

// ── 2. Пустые поля не попадают в профиль ────────────────────────────────────
//
// R134 (доп., 2026-10-16): флажки стали ТРЁХЗНАЧНЫМИ (наследуется / включено /
// выключено). Здесь раньше стояло «не отмеченная галочка → enableReasoning=false»,
// и это был ровно тот дефект, из-за которого применение формы, в которой оператор
// ничего не трогал, молча выключало размышления и mmap в профиле модели. Теперь
// «наследуется» (пустое значение) в профиль НЕ уходит, а явный выбор — уходит.
makeDom([
    { type: 'number', value: '16384', getAttribute: function (k) { return k === 'data-field' ? 'contextLength' : (k === 'data-type' ? 'int' : null); } },
    { type: 'number', value: '', getAttribute: function (k) { return k === 'data-field' ? 'numGpuLayers' : (k === 'data-type' ? 'int' : null); } },
    { type: 'text', value: '', getAttribute: function (k) { return k === 'data-field' ? 'kvCacheType' : (k === 'data-type' ? 'enum' : null); } },
    { type: 'select-one', tagName: 'SELECT', value: 'true', getAttribute: function (k) { return k === 'data-field' ? 'flashAttn' : (k === 'data-type' ? 'bool' : null); } },
    { type: 'select-one', tagName: 'SELECT', value: '', getAttribute: function (k) { return k === 'data-field' ? 'enableReasoning' : (k === 'data-type' ? 'bool' : null); } },
]);
const collected = M.collectModelParams('gemma-4-E4B-it-Q4_K_M');
check('заполненное поле попало в профиль', collected && collected.contextLength === 16384);
check('пустое поле НЕ попало (сохраняется наследование)',
    collected && !('numGpuLayers' in collected) && !('kvCacheType' in collected));
check('явный выбор уходит, «наследуется» — нет',
    collected && collected.flashAttn === true && !('enableReasoning' in collected));

// ── 3. Применение идёт документированным путём ──────────────────────────────
let applyWithProgressCalled = null;
const upserts = [];
global.Api = { cppworkerModelProfiles: { upsert: function (name, body) { upserts.push({ name: name, body: body }); return Promise.resolve({ model: name }); } } };
global.CppWorkerParams = { applyProfileWithProgress: function (name) { applyWithProgressCalled = name; return Promise.resolve(); } };

(async () => {
    const ok = await M.applyModelParams('gemma-4-E4B-it-Q4_K_M');
    check('applyModelParams вернул успех', ok === true);
    check('сначала сохранение профиля на БАЛАНСЕРЕ (не в воркере)', upserts.length === 1 &&
        upserts[0].name === 'gemma-4-E4B-it-Q4_K_M', JSON.stringify(upserts));
    const sent = upserts.length ? upserts[0].body : {};
    check('в тело ушло только заполненное поле', sent.contextLength === 16384 && !('numGpuLayers' in sent));
    check('затем существующий apply с перезагрузкой', applyWithProgressCalled === 'gemma-4-E4B-it-Q4_K_M');

    // Модуль прогресса недоступен: честное сообщение, а не «применили».
    delete global.CppWorkerParams;
    upserts.length = 0;
    toasts.length = 0;
    const ok2 = await M.applyModelParams('gemma-4-E4B-it-Q4_K_M');
    check('без модуля прогресса возвращается false', ok2 === false);
    check('и оператору сказано, что перезагрузка не запущена',
        toasts.some(function (t) { return /вручную|недоступно/i.test(t.msg); }), JSON.stringify(toasts));

    // ── 4. Редактор встроен в обе карточки ──────────────────────────────────
    const fs = require('fs');
    const path = require('path');
    const src = fs.readFileSync(path.join(__dirname, 'gguf-renderer-detail-render.js'), 'utf8');
    const diskPane = src.slice(src.indexOf('function renderModelsPane'), src.indexOf('function exactLayerPlanJS'));
    const loadedPane = src.slice(src.indexOf('function renderLoadedPane'), src.indexOf('function renderDownloadsPane'));
    check('редактор есть в списке моделей на диске (пункт 6)',
        diskPane.indexOf('renderModelParamsEditor') !== -1 && diskPane.indexOf('data-model-acc') !== -1);
    check('редактор есть в карточке загруженной модели (пункт 5)',
        loadedPane.indexOf('renderModelParamsEditor') !== -1 && loadedPane.indexOf('data-model-acc') !== -1);

    // ── 5. Значения загруженной модели берутся из факта, не выдумываются ────
    const fromProv = M.loadedParamsFor(
        { parameters: { contextLength: { value: 12288, source: 'request' }, parallel: { value: 2, source: 'profile' } } },
        { context_size: 65536, kv_cache_type: 'q4_0' });
    check('provenance важнее runtime-флага', fromProv.contextLength === 12288);
    check('runtime-флаг используется, если provenance нет', fromProv.kvCacheType === 'q4_0');
    const empty = M.loadedParamsFor({}, {});
    check('без данных поля пустые (ничего не выдумывается)',
        Object.keys(empty).every(function (k) { return empty[k] === null || empty[k] === undefined; }));

    console.log('\nRESULT: ' + (failures === 0 ? 'PASS' : 'FAIL (' + failures + ')'));
    process.exit(failures === 0 ? 0 : 1);
})();
