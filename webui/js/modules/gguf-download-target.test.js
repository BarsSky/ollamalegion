// gguf-download-target.test.js — R-MultiHost (2026-10-07).
//
// Запуск: node webui/js/modules/gguf-download-target.test.js
//
// ЧТО ПРОВЕРЯЕМ И ПОЧЕМУ.
//
// Операции, которые МЕНЯЮТ ДИСК воркера (скачать модель, узнать прогресс,
// отменить, удалить мусор, список активных загрузок), обязаны уходить на
// ВЫБРАННЫЙ в WebUI бэкенд.
//
// БЫЛО: все они шли «прямым» режимом — URL собирался В БРАУЗЕРЕ как
// http://<host>:<port> из записи бэкенда, и запрос летел с машины оператора
// прямо в воркер, минуя балансер. На стенде из двух машин это давало:
//   - скачивание на удалённый воркер не работало, если host не резолвится из
//     браузера (имя контейнера — типичный случай);
//   - либо уходило НЕ на выбранный воркер: 13.6 GB модели могли лечь на первую
//     машину, хотя оператор выбрал вторую.
//
// СТАЛО: при выбранном бэкенде те же методы идут через прокси балансера
// /api/v1/gguf/backends/{id}/proxy/... — куда доставить, решает балансер по
// своей записи. Пустой selectedBackendId сохраняет прямой режим (кнопка
// «Подключиться» с ручным URL).

'use strict';

const assert = require('assert');

// --- mocks: окружение браузера ----------------------------------------------

global.window = global;
global.localStorage = {
    _d: {},
    getItem: function (k) { return this._d[k] === undefined ? null : this._d[k]; },
    setItem: function (k, v) { this._d[k] = String(v); },
    removeItem: function (k) { delete this._d[k]; },
};
global.WEBUI_CONFIG = {
    API_BASE: 'http://balancer.test:18081',
    API_TOKEN: 'test-token',
    CPPWORKER_URL: 'http://localhost:18092',
};

const requests = [];
let responseFor = function () { return { status: 200, body: {} }; };

global.fetch = function (url, init) {
    const rec = {
        url: String(url),
        method: (init && init.method) || 'GET',
        headers: (init && init.headers) || {},
        body: init && init.body ? JSON.parse(init.body) : null,
    };
    requests.push(rec);
    const r = responseFor(rec);
    return Promise.resolve({
        ok: r.status < 400,
        status: r.status,
        headers: { get: function () { return 'application/json'; } },
        text: function () { return Promise.resolve(JSON.stringify(r.body)); },
        json: function () { return Promise.resolve(r.body); },
    });
};

require('./gguf-api.js');
const api = global.GgufApi;
assert.ok(api, 'window.GgufApi должен быть экспортирован');
assert.strictEqual(typeof api.setBackendId, 'function', 'setBackendId должен быть в публичной поверхности');

// --- helpers ----------------------------------------------------------------

function only() {
    assert.strictEqual(requests.length, 1, 'ожидался ровно один запрос, получено ' + requests.length);
    return requests[0];
}

function reset() {
    requests.length = 0;
    responseFor = function () { return { status: 200, body: {} }; };
}

const PROXY = 'http://balancer.test:18081/api/v1/gguf/backends/CPPWORKER-34/proxy';

// --- 1. Выбран бэкенд → всё идёт через прокси балансера ---------------------

async function testDownloadGoesThroughSelectedBackend() {
    api.setBackendId('CPPWORKER-34');
    reset();

    await api.startDownload('Qwen/Qwen2.5-0.5B-GGUF', 'qwen2.5-0.5b-q4_k_m.gguf');

    const r = only();
    assert.strictEqual(r.method, 'POST');
    assert.strictEqual(r.url.split('?')[0], PROXY + '/api/hf/download');
    assert.deepStrictEqual(r.body, {
        modelId: 'Qwen/Qwen2.5-0.5B-GGUF',
        filename: 'qwen2.5-0.5b-q4_k_m.gguf',
        revision: 'main',
    });
    // Токен балансера обязателен: прокси-путь под AuthMiddleware.
    assert.strictEqual(r.headers['X-API-Token'], 'test-token');
    console.log('  ok: startDownload уходит на выбранный бэкенд через балансер');
}

async function testProgressAndCancelAndCleanupUseBackend() {
    api.setBackendId('CPPWORKER-34');

    reset();
    await api.getDownloadProgress('repo/model', 'file.gguf');
    let r = only();
    assert.ok(r.url.indexOf(PROXY + '/api/hf/progress?') === 0,
        'прогресс должен читаться у выбранного бэкенда, получено ' + r.url);
    assert.ok(r.url.indexOf('modelId=repo%2Fmodel') !== -1, 'modelId должен быть закодирован');
    console.log('  ok: getDownloadProgress уходит на выбранный бэкенд');

    reset();
    await api.listActiveDownloads();
    r = only();
    assert.strictEqual(r.url.split('?')[0], PROXY + '/api/hf/downloads');
    console.log('  ok: listActiveDownloads уходит на выбранный бэкенд');

    reset();
    await api.cancelDownload('repo/model', 'file.gguf');
    r = only();
    assert.strictEqual(r.url.split('?')[0], PROXY + '/api/hf/cancel');
    assert.strictEqual(r.method, 'POST');
    console.log('  ok: cancelDownload уходит на выбранный бэкенд');

    reset();
    await api.deleteDownload('repo/model', 'file.gguf');
    r = only();
    assert.strictEqual(r.url.split('?')[0], PROXY + '/api/hf/cleanup');
    assert.strictEqual(r.method, 'POST');
    console.log('  ok: deleteDownload (чистка мусора) уходит на выбранный бэкенд');
}

// --- 2. Бэкенд не выбран → прежний прямой режим -----------------------------

async function testDirectModeWithoutSelectedBackend() {
    api.setBackendId('');
    assert.strictEqual(api.getBackendId(), '', 'getBackendId должен вернуть пусто');

    reset();
    await api.startDownload('repo/model', 'file.gguf');
    const r = only();
    assert.strictEqual(r.url, 'http://localhost:18092/api/hf/download',
        'без выбранного бэкенда должен остаться прямой режим (кнопка «Подключиться»)');
    console.log('  ok: без выбранного бэкенда сохранён прямой режим');
}

// --- 3. Смена бэкенда переключает адресата ----------------------------------

async function testSwitchingBackendChangesTarget() {
    api.setBackendId('cppworker-gpu-bundled-agent');
    reset();
    await api.listActiveDownloads();
    const first = only().url;
    assert.ok(first.indexOf('/api/v1/gguf/backends/cppworker-gpu-bundled-agent/proxy/') !== -1,
        'первый адресат — выбранный бэкенд, получено ' + first);

    api.setBackendId('CPPWORKER-34');
    reset();
    await api.listActiveDownloads();
    const second = only().url;
    assert.ok(second.indexOf('/api/v1/gguf/backends/CPPWORKER-34/proxy/') !== -1,
        'после смены выбора адресат должен смениться, получено ' + second);
    assert.notStrictEqual(first, second);
    console.log('  ok: смена выбранного бэкенда меняет адресата операций с диском');
}

// --- 4. Ошибка прокси несёт код и цель --------------------------------------

async function testProxyErrorCarriesTarget() {
    api.setBackendId('CPPWORKER-34');
    reset();
    responseFor = function () { return { status: 404, body: { error: 'Backend not found' } }; };

    let caught = null;
    try {
        await api.startDownload('repo/model', 'file.gguf');
    } catch (e) {
        caught = e;
    }
    assert.ok(caught, 'ошибка должна пробрасываться');
    assert.strictEqual(caught.status, 404);
    assert.strictEqual(caught.code, 'not_found');
    assert.ok(caught.target && caught.target.indexOf('/gguf/backends/CPPWORKER-34/') !== -1,
        'в ошибке должен быть адресат, иначе непонятно, кого не нашли');
    console.log('  ok: ошибка прокси содержит код и адресата');
}

// --- run --------------------------------------------------------------------

(async function () {
    await testDownloadGoesThroughSelectedBackend();
    await testProgressAndCancelAndCleanupUseBackend();
    await testDirectModeWithoutSelectedBackend();
    await testSwitchingBackendChangesTarget();
    await testProxyErrorCarriesTarget();
    console.log('gguf-download-target.test.js: все проверки пройдены');
})().catch(function (e) {
    console.error('FAILED: ' + (e && e.stack || e));
    process.exit(1);
});
