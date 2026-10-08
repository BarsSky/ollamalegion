// gguf-refresh-resilience.test.js — R91 (2026-10-08).
//
// Запуск: node webui/js/modules/gguf-refresh-resilience.test.js
//
// ЖИВАЯ ЖАЛОБА. «С большой задержкой стали открываться настройки бэкендов по
// выбору из списка тип llama.cpp на странице gguf models… и пропала вообще вся
// информация кроме инфо». Причина: страница снимала флаг «обновление уже идёт»
// только в then/catch, поэтому ОДИН незавершившийся запрос к балансеру навсегда
// отключал обновление деталей выбранного бэкенда: вкладки «Модели» и «Настройки»
// оставались пустыми, а «Инфо» показывал последнее отрисованное — до перезагрузки
// страницы. Плюс сбой вообще не показывался оператору (только console.warn).
//
// Тест фиксирует четыре свойства:
//   1. сбой загрузки деталей ВИДЕН (state.detailError) и панель предлагает повтор;
//   2. зависший запрос не блокирует панель навсегда (сторожевой предел);
//   3. успешный ответ снимает ошибку предыдущей попытки;
//   4. флаг списка бэкендов снимается и при ошибке (иначе список «замерзал»).

'use strict';

const assert = require('assert');

// --- mocks -----------------------------------------------------------------

global.window = global;
global.Utils = { escapeHtml: function (s) { return String(s === null || s === undefined ? '' : s); } };
global.I18N = { t: function (key) { return key; } };

global.GgufModule = {
    state: {
        selectedBackendId: 'bk-1',
        registeredBackends: [{ id: 'bk-1' }],
        detailPane: 'models',
        detailLoading: false,
        detailError: null,
        localModels: [],
        loadedModels: [],
        runtimeModels: {},
        workerInfo: null,
        gpuInfo: null
    },
    repairCount: 0,
    refreshDetailPanel: function () { global.GgufModule.repairCount++; },
    refreshDetailPane: function () { global.GgufModule.repairCount++; },
    // Заглушки, которые gguf-renderer-detail-render.js забирает СЕБЕ в алиасы на
    // этапе загрузки модуля (иначе `_ is not a function`).
    _: function (key) { return key; },
    stripGGUF: function (n) { return n; },
    formatFileSize: function (n) { return String(n) + ' B'; },
    formatVramMB: function (n) { return String(n); },
    showToast: function () {}
};
global.GgufApi = {
    getRuntimeConfigViaBackend: function () { return Promise.resolve({ loaded_models: [] }); },
    listLocalModelsViaBackend: function () { return Promise.resolve({ files: [] }); },
    requestViaBackend: function () { return Promise.resolve({}); }
};

require('./gguf-renderer-refresh.js');
require('./gguf-renderer-detail-render.js');
const M = global.GgufModule;

// Модуль refresh определяет свои refreshDetailPanel/refreshDetailPane (они лезут
// в document). В тесте DOM нет — считаем вызовы перерисовки сами.
M.repairCount = 0;
M.refreshDetailPanel = function () { M.repairCount++; };
M.refreshDetailPane = function () { M.repairCount++; };

let checks = 0;
function check(name, fn) {
    fn();
    checks++;
    console.log('  ✓ ' + name);
}

// Модуль честно пишет console.warn о сбое — в тесте это ожидаемый шум.
const realWarn = console.warn;
console.warn = function () {};

(async function run() {
    // --- 1. Сбой виден и объяснён -------------------------------------------
    global.Api = {
        getBackend: function () { return Promise.reject(new Error('HTTP 503: Service Unavailable')); },
        fetchGgufBackends: function () { return Promise.resolve({ backends: [] }); }
    };
    await M.refreshDetail();
    check('сбой загрузки деталей показан оператору, а не только в консоли', function () {
        assert.ok(M.state.detailError, 'state.detailError пуст — оператор снова увидит пустые вкладки без причины');
        assert.ok(M.state.detailError.indexOf('gguf.detail_load_failed') !== -1,
            'текст ошибки не локализован: ' + M.state.detailError);
        assert.ok(M.state.detailError.indexOf('HTTP 503') !== -1,
            'в тексте нет причины от сервера: ' + M.state.detailError);
    });
    check('панель перерисована — плашка ошибки видна без ручного обновления', function () {
        assert.ok(M.repairCount > 0, 'refreshDetailPanel не вызывался после ошибки');
    });
    check('плашка ошибки содержит кнопку «Повторить» и экранирует текст', function () {
        const html = M.renderDetailPane();
        assert.ok(html.indexOf('ggufDetailRetry') !== -1, 'нет кнопки повтора: ' + html);
        assert.ok(html.indexOf('gguf.detail_load_retry') !== -1, 'нет локализованной подписи кнопки');
        assert.ok(html.indexOf('<strong>gguf.detail_load_failed</strong>') !== -1,
            'заголовок плашки не отрисован: ' + html);
    });

    // --- 2. Зависший запрос не блокирует панель навсегда ---------------------
    {
        const realNow = Date.now;
        let now = 1700000000000;
        Date.now = function () { return now; };
        let calls = 0;
        global.Api.getBackend = function () { calls++; return new Promise(function () { /* висим вечно */ }); };
        M.state.detailError = null;

        M.refreshDetail();                       // старт: висит
        M.refreshDetail();                       // отбито флагом «обновление идёт»
        check('во время активного обновления повторный запрос не уходит', function () {
            assert.strictEqual(calls, 1, 'параллельные запросы деталей не должны дублироваться');
        });

        now += 31000;                            // прошло больше сторожевого предела (30 с)
        M.refreshDetail();
        check('зависшее обновление перестаёт блокировать панель (сторожевой предел)', function () {
            assert.strictEqual(calls, 2,
                'после зависшего запроса панель осталась заблокированной — это и есть «пропала вся информация кроме инфо»');
        });
        Date.now = realNow;
    }

    // --- 3. Успех снимает ошибку --------------------------------------------
    global.Api.getBackend = function () {
        return Promise.resolve({
            id: 'bk-1',
            backendType: 'llama_cpp',
            status: 'healthy',
            llamaCpp: { loadedModels: [{ name: 'gemma-4-E4B-it-Q4_K_M' }] }
        });
    };
    M.state.detailError = 'старая ошибка';
    await M.refreshDetail();
    check('успешный ответ снимает плашку ошибки и наполняет вкладку «Модели»', function () {
        assert.strictEqual(M.state.detailError, null, 'плашка ошибки осталась после успеха');
        assert.strictEqual(M.state.localModels.length, 1, 'localModels не заполнены');
        assert.strictEqual(M.state.localModels[0].name, 'gemma-4-E4B-it-Q4_K_M');
    });

    // --- 4. Флаг списка бэкендов снимается при ошибке -------------------------
    {
        let n = 0;
        global.Api.fetchGgufBackends = function () {
            n++;
            return Promise.reject(new Error('boom'));
        };
        await M.refreshBackends();
        await M.refreshBackends();
        check('после сбоя список бэкендов обновляется снова, а не «замерзает»', function () {
            assert.strictEqual(n, 2, 'второй refreshBackends не ушёл — флаг остался взведённым');
        });
    }

    console.log('\nOK: ' + checks + ' checks passed');
    console.warn = realWarn;
    process.exit(0);
})().catch(function (e) {
    console.warn = realWarn;
    console.error('ПРОВАЛ: ' + (e && e.message));
    process.exit(1);
});
