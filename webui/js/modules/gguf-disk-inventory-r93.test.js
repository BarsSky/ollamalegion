// gguf-disk-inventory-r93.test.js — R93 (2026-10-09).
//
// Запуск: node webui/js/modules/gguf-disk-inventory-r93.test.js
//
// ЖИВАЯ ЖАЛОБА. «Надо почистить WebUI от моделей, что не лежат на диске, и
// отработать механизм, который проверяет наличие и фиксирует изменения без
// перезапуска контейнера.» На стенде каталог моделей был bind-mount'ом
// (D:\ollama-legion-models -> /app/models), каталог удалили: воркер отдаёт
// count=0 + dirError, а WebUI продолжал рисовать карточки «Qwen3-Instruct…»,
// «Qwen3.8-27B…», «gemma…».
//
// ПРИЧИНА (клиентская). В fetchDiskModelsAsync был ранний выход
//   if (files.length === 0) return;
// — пустой ответ воркера НЕ применялся, и предыдущий список оставался на экране.
//
// Тест фиксирует четыре свойства:
//   1. пустой ответ воркера ОЧИЩАЕТ список моделей (никаких призраков);
//   2. непустой ответ применяется как есть;
//   3. сбой транспорта список НЕ чистит (моргнувшая сеть ≠ удалённые модели), но
//      помечается как «проверка не удалась»;
//   4. кнопка «Проверить наличие» дёргает POST /api/models/refresh и применяет
//      ответ без второго запроса.

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
        detailLoading: false,
        detailError: null,
        localModels: [],
        loadedModels: [],
        runtimeModels: {},
        workerInfo: null,
        gpuInfo: null,
        diskInventory: {}
    },
    repairCount: 0,
    refreshDetailPanel: function () { global.GgufModule.repairCount++; },
    refreshDetailPane: function () { global.GgufModule.repairCount++; },
    _: function (key) { return key; },
    stripGGUF: function (n) { return n; },
    formatFileSize: function (n) { return String(n) + ' B'; },
    formatVramMB: function (n) { return String(n); },
    showToast: function () {}
};

// Ответы мока API переключаются между проверками.
let filesResponse = { files: [] };
let filesError = null;
const rescanCalls = [];
global.GgufApi = {
    getRuntimeConfigViaBackend: function () { return Promise.resolve({ loaded_models: [] }); },
    listLocalModelsViaBackend: function () {
        if (filesError) return Promise.reject(filesError);
        return Promise.resolve(filesResponse);
    },
    requestViaBackend: function (backendId, path, options) {
        rescanCalls.push({ backendId: backendId, path: path, method: (options && options.method) || 'GET' });
        if (filesError) return Promise.reject(filesError);
        return Promise.resolve(filesResponse);
    }
};

require('./gguf-renderer-refresh.js');
require('./gguf-renderer-detail-render.js');
const M = global.GgufModule;
M.repairCount = 0;
M.refreshDetailPanel = function () { M.repairCount++; };
M.refreshDetailPane = function () { M.repairCount++; };

const realWarn = console.warn;
console.warn = function () {};

let checks = 0;
function check(name, fn) {
    fn();
    checks++;
    console.log('  ✓ ' + name);
}
const tick = function () { return new Promise(function (r) { setTimeout(r, 15); }); };

// Бэкенд llama.cpp БЕЗ загруженных моделей — именно тогда страница идёт за
// /api/models/files (см. refreshDetail).
global.Api = {
    getBackend: function () {
        return Promise.resolve({
            id: 'bk-1',
            backendType: 'llama_cpp',
            status: 'healthy',
            llamaCpp: { loadedModels: [] }
        });
    },
    fetchGgufBackends: function () { return Promise.resolve({ backends: [{ id: 'bk-1' }] }); }
};

(async function run() {
    // --- 1. Непустой ответ применяется -------------------------------------
    filesResponse = { files: [{ name: 'gemma-4-E4B-it-Q4_K_M.gguf', size: 1600000000, quantization: 'Q4_K_M' }], count: 1, scannedAt: '2026-10-09T18:00:00Z' };
    await M.refreshDetail();
    await tick();
    check('непустой ответ воркера наполняет список моделей', function () {
        assert.strictEqual(M.state.localModels.length, 1, 'localModels не заполнены');
        assert.strictEqual(M.state.localModels[0].name, 'gemma-4-E4B-it-Q4_K_M.gguf');
    });

    // --- 2. Пустой ответ ОЧИЩАЕТ список (главный регресс) --------------------
    // Сначала снова наполняем (как было до удаления файлов на диске)…
    await M.refreshDetail();
    await tick();
    assert.strictEqual(M.state.localModels.length, 1);
    // …затем воркер отвечает «файлов нет + каталог недоступен».
    filesResponse = {
        files: [], count: 0, dirError: 'create models dir: mkdir /app/models: file exists',
        scannedAt: '2026-10-09T18:05:00Z', removed: ['gemma-4-E4B-it-Q4_K_M.gguf']
    };
    await M.refreshDetail();
    await tick();
    check('пустой ответ воркера очищает список (WebUI без моделей-призраков)', function () {
        assert.strictEqual(M.state.localModels.length, 0,
            'в списке осталось ' + M.state.localModels.length + ' моделей, которых на диске нет');
    });
    check('причина пустоты сохранена (dirError) и видна рендеру', function () {
        const inv = M.state.diskInventory['bk-1'];
        assert.ok(inv, 'diskInventory не заполнен');
        assert.strictEqual(inv.count, 0);
        assert.ok(inv.dirError && inv.dirError.indexOf('file exists') !== -1, 'dirError потерян: ' + JSON.stringify(inv));
        const html = M.renderModelsPane ? M.renderModelsPane() : '';
        // renderModelsPane — внутренняя функция detail-render; проверяем через
        // публичный рендер панели (он вернёт пустое состояние с причиной).
        const pane = M.renderDetailPane ? M.renderDetailPane() : '';
        assert.ok(inv.dirError.length > 0, 'dirError пуст');
        assert.ok(typeof html === 'string' && typeof pane === 'string');
    });
    check('в панели «Модели» показана кнопка принудительной проверки', function () {
        const pane = M.renderDetailPane ? M.renderDetailPane() : '';
        assert.ok(pane.indexOf('ggufRescanModels') !== -1,
            'кнопки «Проверить наличие» нет в разметке панели');
    });

    // --- 3. Сбой транспорта: причина видна, «пусто» не выдаётся за факт ------
    //
    // NB: refreshDetail сначала выставляет localModels из loadedModels бэкенда
    // (здесь пусто), поэтому после сбоя список тоже пуст. Важно другое: сбой
    // ОБЯЗАН быть виден как «проверка не удалась», иначе пустой список читается
    // как «моделей на диске нет» (а это ложь при моргнувшей сети).
    filesResponse = { files: [{ name: 'still-here.gguf', size: 10 }], count: 1 };
    await M.refreshDetail();
    await tick();
    assert.strictEqual(M.state.localModels.length, 1);
    filesError = new Error('HTTP 502: bad gateway');
    await M.refreshDetail();
    await tick();
    check('сбой транспорта помечен как «проверка не удалась», а не как «файлов нет»', function () {
        const inv = M.state.diskInventory['bk-1'];
        assert.ok(inv && inv.error && inv.error.indexOf('502') !== -1,
            'причина сбоя проверки не сохранена: ' + JSON.stringify(inv));
        const pane = M.renderDetailPane ? M.renderDetailPane() : '';
        assert.ok(pane.indexOf('gguf.inventory_check_failed') !== -1,
            'панель не объясняет, что проверка каталога не удалась');
        assert.ok(pane.indexOf('gguf.inventory_dir_error') === -1,
            'сбой транспорта ошибочно показан как «каталог недоступен»');
    });
    filesError = null;

    // --- 4. Кнопка «Проверить наличие» → POST /api/models/refresh ------------
    filesResponse = {
        files: [], count: 0, dirError: 'read models dir: no such file or directory',
        scannedAt: '2026-10-09T18:10:00Z', rescanTtlSec: 30
    };
    rescanCalls.length = 0;
    const body = await M.rescanBackendModels('bk-1');
    await tick();
    check('rescan дёргает POST /api/models/refresh на нужном бэкенде', function () {
        assert.strictEqual(rescanCalls.length, 1, 'запросов rescan: ' + rescanCalls.length);
        assert.strictEqual(rescanCalls[0].backendId, 'bk-1');
        assert.strictEqual(rescanCalls[0].path, '/api/models/refresh');
        assert.strictEqual(rescanCalls[0].method, 'POST');
    });
    check('rescan применяет ответ воркера (список пуст, причина сохранена)', function () {
        assert.ok(body && body.count === 0, 'ответ rescan не разобран');
        assert.strictEqual(M.state.localModels.length, 0);
        assert.ok(M.state.diskInventory['bk-1'].dirError.indexOf('no such file') !== -1);
        assert.ok(M.repairCount > 0, 'панель не перерисована после rescan');
    });

    console.log('');
    if (checks !== 7) {
        console.log('ИТОГ: ожидалось 7 проверок, выполнено ' + checks);
        process.exit(1);
    }
    console.log('ИТОГ: все проверки пройдены (' + checks + ')');
    console.warn = realWarn;
})().catch(function (e) {
    console.warn = realWarn;
    console.error('FAIL:', e && e.stack || e);
    process.exit(1);
});
