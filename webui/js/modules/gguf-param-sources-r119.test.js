// gguf-param-sources-r119.test.js — R119 (2026-10-15), пункт 4 (UI-половина).
//
// ЗАЧЕМ. Оператор видел только ЗНАЧЕНИЯ параметров загруженной модели и не мог
// понять, какой слой их поставил (явный запрос клиента / per-model профиль /
// «настройки по умолчанию» / env cppworker). Балансер теперь отдаёт источник
// (loadedModels[].parameters = {param: {value, source}}), и карточка модели на
// странице GGUF обязана показать его рядом со значением.
//
// Проверяются чистые функции (без DOM): порядок параметров, человеческие метки
// источников, прочерк для nil («никто не задал»), экранирование и отсутствие
// домыслов, когда provenance нет вовсе.
//
// Запуск: node webui/js/modules/gguf-param-sources-r119.test.js
'use strict';

const assert = require('assert');

global.window = global;
global.Utils = {
    escapeHtml: function (s) {
        return String(s === null || s === undefined ? '' : s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    },
};
global.I18N = { t: function (key) { return key; } };

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
    showToast: function () {},
    refreshDetailPane: function () {},
    refreshDetailPanel: function () {},
};
global.GgufApi = {
    getRuntimeConfigViaBackend: function () { return Promise.resolve({ loaded_models: [] }); },
    listLocalModelsViaBackend: function () { return Promise.resolve({ files: [] }); },
    requestViaBackend: function () { return Promise.resolve({ files: [] }); },
};

require('./gguf-renderer-detail-render.js');
const M = global.GgufModule;

let failures = 0;
function check(name, cond, detail) {
    if (cond) { console.log('  PASS ' + name); }
    else { failures++; console.log('  FAIL ' + name + (detail ? ' — ' + detail : '')); }
}

console.log('=== R119: источники параметров загрузки в UI ===\n');

check('модуль экспортирует buildParamSources', typeof M.buildParamSources === 'function');
check('модуль экспортирует renderParamSources', typeof M.renderParamSources === 'function');

const params = {
    contextLength: { value: 12288, source: 'request' },
    batchSize: { value: 512, source: 'profile' },
    kvCacheType: { value: 'q4_0', source: 'profile' },
    flashAttn: { value: 1, source: 'default_profile' },
    parallel: { value: null, source: 'backend' },
};
const items = M.buildParamSources(params);

check('все параметры разобраны', items.length === 5, JSON.stringify(items.map(function (i) { return i.key; })));
check('contextLength идёт первым (порядок для чтения)',
    items[0].key === 'contextLength' && items[0].value === '12288' && items[0].sourceLabel === 'запрос',
    JSON.stringify(items[0]));
check('профиль подписан по-человечески',
    items.filter(function (i) { return i.sourceLabel === 'профиль'; }).length === 2,
    JSON.stringify(items));
check('«настройки по умолчанию» различимы от профиля',
    items.some(function (i) { return i.key === 'flashAttn' && i.sourceLabel === 'по умолчанию'; }));
check('nil-значение показывается прочерком, а не пустотой',
    items.some(function (i) { return i.key === 'parallel' && i.value === '—' && i.sourceLabel === 'cppworker'; }));
check('у каждого источника есть подсказка для оператора',
    items.every(function (i) { return typeof i.title === 'string'; }));

check('без provenance — пусто (никаких домыслов)',
    M.buildParamSources(null).length === 0 && M.buildParamSources(undefined).length === 0 &&
    M.buildParamSources({}).length === 0);

// HTML-рендер: значение и метка экранированы, метка источника видна.
const html = M.renderParamSources({
    contextLength: { value: 8192, source: 'profile' },
    kvCacheType: { value: '<script>', source: 'request' },
});
check('рендер содержит значение и метку источника', html.indexOf('8192') >= 0 && html.indexOf('профиль') >= 0, html);
check('рендер экранирует значение', html.indexOf('<script>') < 0 && html.indexOf('&lt;script&gt;') >= 0, html);
check('без provenance рендер пуст', M.renderParamSources(null) === '');

// Карточка загруженной модели обязана показывать источник рядом со значением.
M.state.loadedModels = [{
    name: 'gemma-4-E4B-it-Q4_K_M',
    contextLength: 12288,
    parameters: { contextLength: { value: 12288, source: 'request' }, parallel: { value: 2, source: 'profile' } },
}];
M.state.runtimeModels = {};
const pane = M.renderLoadedPane();
check('карточка загруженной модели содержит блок источников',
    pane.indexOf('gguf.param_sources') >= 0 && pane.indexOf('профиль') >= 0 && pane.indexOf('запрос') >= 0 &&
    pane.indexOf('contextLength=12288') >= 0,
    pane.slice(0, 600));

console.log('\n' + (failures === 0 ? 'RESULT: PASS' : 'RESULT: FAIL (' + failures + ')'));
process.exit(failures === 0 ? 0 : 1);
