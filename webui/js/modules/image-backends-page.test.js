// image-backends-page.test.js - R-Image: интеграционный smoke страницы
// «Image-бэкенды» (управление бэкендами генерации изображений, тип image_cpp)
// с мок-DOM, мок-fetch и реальным паком переводов ru.js (без браузера).
//
// Запуск: node webui/js/modules/image-backends-page.test.js
//
// ЧТО ПРОВЕРЯЕМ (то, что не поймать чистым юнит-тестом):
//   1. Разметка и JS не разъехались: каждый id, за которым модуль ходит через
//      byId/getElementById, есть в webui/index.html (или генерируется самим
//      модулем - id="ib..."), а пункт навигации/страница/скрипт объявлены.
//   2. Таблица заполняется данными воркера: порт 18093, состояние, текущая
//      модель, VRAM и счётчики запросов total/ok/failed/rejected/in-flight.
//   3. syncVisibility: пункт навигации и страница видны при наличии image_cpp
//      и скрыты, когда image-бэкендов нет.
//   4. Валидация формы добавления: пустой id/host и битый порт дают ровно те
//      тексты ошибок, что лежат в i18n (и запрос на сервер НЕ уходит).
//   5. CRUD: POST (создание), PUT (gpuIndex: нет ключа / null / число),
//      DELETE по кнопке «Удалить» (с подтверждением и без него).
//   6. Модели воркера: load/unload уходят на /api/v1/image/backends/{id}/models/...
//      а неизвестная текущая модель берётся из GET .../models.
//   7. Пустой список рендерит imageBackends.empty, а не пустую таблицу.
//   8. Все i18n-ключи, которые дёргает модуль, есть в ru.js.

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

let passed = 0;
const failures = [];
// check() умеет и sync-, и async-случаи: await на каждом вызове гарантирует,
// что проверки идут ПО ПОРЯДКУ (следующая видит состояние после предыдущей).
async function check(name, fn) {
    try {
        await fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' - ' + (e && e.message));
    }
}

// --- mocks: window / localStorage / i18n -----------------------------------
global.window = global;
const i18nListeners = [];
global.addEventListener = function (type, fn) {
    if (type === 'i18n:changed' && typeof fn === 'function') i18nListeners.push(fn);
};
global.removeEventListener = function () {};

const store = {};
global.localStorage = {
    getItem: function (k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem: function (k, v) { store[k] = String(v); },
    removeItem: function (k) { delete store[k]; },
};

// Реальный ru.js: тест заодно проверяет, что все ключи модуля переведены.
require('../i18n/ru.js');
const RU = global.I18N_RU;
assert.ok(RU, 'window.I18N_RU должен быть загружен из ru.js');
const missingKeys = [];
global.I18N = {
    getLang: function () { return 'ru'; },
    t: function (key, vars) {
        let s = RU[key];
        if (s === undefined) {
            if (missingKeys.indexOf(key) === -1) missingKeys.push(key);
            s = key;
        }
        if (vars) {
            Object.keys(vars).forEach(function (p) { s = String(s).replace('{' + p + '}', vars[p]); });
        }
        return s;
    },
};

const toasts = [];
global.showToast = function (msg, type) { toasts.push({ msg: msg, type: type }); };

global.WEBUI_CONFIG = { API_BASE: 'http://balancer.test:18081', API_TOKEN: 'test-token' };

let confirmAnswer = true;
global.confirm = function () { return confirmAnswer; };
let promptAnswer = '';
global.prompt = function () { return promptAnswer; };

// --- mocks: fetch / Api -----------------------------------------------------
const requests = [];

function jsonResponse(body, status) {
    const s = status || 200;
    return Promise.resolve({
        ok: s < 400,
        status: s,
        headers: { get: function () { return null; } },
        text: function () { return Promise.resolve(body === undefined ? '' : JSON.stringify(body)); },
        json: function () { return Promise.resolve(body); },
    });
}

// Полезная нагрузка GET /api/v1/cluster (контракт зафиксирован задачей).
function clusterPayload() {
    return {
        backends: [
            {
                id: 'imageworker', backendType: 'image_cpp', status: 'healthy',
                host: 'imageworker', imagePort: 18093, maxConcurrentRequests: 64,
                gpuIndex: 0,
                image: {
                    state: 'loaded', currentModel: 'sd15-q4', vramFreeMb: 894, vramTotalMb: 8192,
                    updatedAt: '2026-10-03T10:00:00Z', lastError: '',
                    models: [{ name: 'sd15-q4', state: 'loaded', family: 'sd15', sizeBytes: 1526, vramEstimateMb: 2600, activeQueries: 0 }],
                    requests: {
                        inFlight: 0, total: 7, ok: 6, failed: 1, rejected: 2, accepted: 0, finished: 0,
                        rps: 0.1, avgDurationMs: 9100, p50DurationMs: 8800, p95DurationMs: 21000,
                        lastDurationMs: 9000, lastRequestAt: '2026-10-03T10:00:00Z',
                        failuresByCode: { engine_oom: 1 }, gateDeniedByCode: { insufficient_vram: 2 },
                    },
                },
            },
            // Текстовый бэкенд: на странице он появляться не должен.
            { id: 'llama-1', backendType: 'llama_cpp', status: 'healthy', host: 'cppworker', cppWorkerPort: 18080 },
        ],
    };
}

let clusterBackends = clusterPayload().backends;

function route(url, init) {
    const method = (init && init.method) || 'GET';
    const p = String(url).replace(/^https?:\/\/[^/]+/, '').split('?')[0];
    const body = (init && init.body) ? JSON.parse(init.body) : null;
    requests.push({ url: String(url), path: p, method: method, body: body, headers: (init && init.headers) || {} });

    if (p === '/api/v1/cluster' && method === 'GET') return jsonResponse({ backends: clusterBackends });
    // Политика сосуществования и лимиты гейта: страница берёт их из метрик
    // (в /api/v1/cluster этих полей нет).
    if (p === '/api/v1/metrics' && method === 'GET') {
        return jsonResponse({
            image: {
                coexistence_policy: 'exclusive', vram_headroom_mb: 512,
                queue_wait_timeout_sec: 60, exclusive_lock_timeout_sec: 120,
                gate_disabled: false,
            },
        });
    }
    if (p === '/api/v1/backends' && method === 'GET') return jsonResponse({ backends: clusterBackends, total: clusterBackends.length });
    if (p === '/api/v1/backends' && method === 'POST') return jsonResponse({ status: 'ok' });
    if (/^\/api\/v1\/backends\/[^/]+$/.test(p) && (method === 'PUT' || method === 'DELETE')) return jsonResponse({ status: 'ok' });
    if (/^\/api\/v1\/image\/backends\/[^/]+\/models$/.test(p) && method === 'GET') {
        return jsonResponse({ models: [{ name: 'sd15-q4', state: 'not_loaded' }, { name: 'sdxl-base', state: 'not_loaded' }] });
    }
    if (/^\/api\/v1\/image\/backends\/[^/]+\/models\/(load|unload)$/.test(p) && method === 'POST') {
        if (!body || !body.name) return jsonResponse({ error: 'name is required' }, 400);
        return jsonResponse({ status: 'ok', model: body.name });
    }
    return jsonResponse({ error: 'unexpected ' + method + ' ' + p }, 404);
}

global.fetch = function (url, init) { return route(url, init); };

// Api повторяет контракт api.js: методы бросают на !ok и возвращают response.ok.
function apiRequest(p, method, body) {
    return global.fetch(global.WEBUI_CONFIG.API_BASE + p, {
        method: method,
        body: body === undefined ? undefined : JSON.stringify(body),
        headers: { 'Content-Type': 'application/json', 'X-API-Token': 'test-token' },
    }).then(function (resp) {
        if (!resp.ok) {
            const err = new Error('HTTP ' + resp.status);
            err.status = resp.status;
            throw err;
        }
        return resp;
    });
}

global.Api = {
    getAuthHeaders: function () {
        return { 'Content-Type': 'application/json', 'X-API-Token': 'test-token' };
    },
    cluster: function () { return apiRequest('/api/v1/cluster', 'GET').then(function (r) { return r.json(); }); },
    backends: function () { return apiRequest('/api/v1/backends', 'GET').then(function (r) { return r.json(); }); },
    createBackend: function (data) { return apiRequest('/api/v1/backends', 'POST', data).then(function (r) { return r.ok; }); },
    updateBackend: function (id, data) { return apiRequest('/api/v1/backends/' + encodeURIComponent(id), 'PUT', data).then(function (r) { return r.ok; }); },
    deleteBackend: function (id) { return apiRequest('/api/v1/backends/' + encodeURIComponent(id), 'DELETE').then(function (r) { return r.ok; }); },
    post: function (p, data) { return apiRequest(p, 'POST', data).then(function (r) { return r.json(); }); },
};

// --- inventory: исходник модуля / index.html --------------------------------
const MODULE_SRC = fs.readFileSync(path.join(__dirname, 'image-backends-page.js'), 'utf8');
const INDEX_HTML = fs.readFileSync(path.join(__dirname, '..', '..', 'index.html'), 'utf8');
const APP_SRC = fs.readFileSync(path.join(__dirname, '..', 'app.js'), 'utf8');

const HTML_IDS = new Set();
{
    const re = /id="([A-Za-z0-9_-]+)"/g;
    let m;
    while ((m = re.exec(INDEX_HTML)) !== null) HTML_IDS.add(m[1]);
}

// id, которые модуль создаёт САМ в модалке редактора (их нет в index.html).
const GENERATED_IDS = new Set();
{
    const re = /\bid="(ib[A-Za-z0-9]*)"/g;
    let m;
    while ((m = re.exec(MODULE_SRC)) !== null) GENERATED_IDS.add(m[1]);
}

// Литеральные id, за которыми модуль ходит через byId/getElementById.
const LITERAL_IDS = new Set();
{
    [/getElementById\('([A-Za-z0-9_-]+)'\)/g, /\bbyId\('([A-Za-z0-9_-]+)'\)/g].forEach(function (re) {
        let m;
        while ((m = re.exec(MODULE_SRC)) !== null) LITERAL_IDS.add(m[1]);
    });
}

// Константы вида var PAGE_ID = 'image-backends-page';
const MODULE_CONSTS = {};
{
    const re = /var\s+([A-Z][A-Z0-9_]*)\s*=\s*'([A-Za-z0-9_-]+)'/g;
    let m;
    while ((m = re.exec(MODULE_SRC)) !== null) MODULE_CONSTS[m[1]] = m[2];
}

// --- mocks: DOM -------------------------------------------------------------
const elements = {};
function makeElement(id) {
    const classes = new Set();
    const el = {
        id: id,
        style: {},
        _html: '',
        textContent: '',
        value: '',
        disabled: false,
        options: [],
        _listeners: {},
        _attrs: {},
        _clicks: 0,
        addEventListener: function (type, fn) { (this._listeners[type] = this._listeners[type] || []).push(fn); },
        removeEventListener: function () {},
        dispatch: function (type, ev) { (this._listeners[type] || []).forEach(function (fn) { fn(ev || {}); }); },
        click: function () { this._clicks++; this.dispatch('click', { target: this }); },
        getAttribute: function (n) { return Object.prototype.hasOwnProperty.call(this._attrs, n) ? this._attrs[n] : null; },
        setAttribute: function (n, v) { this._attrs[n] = v; },
        querySelector: function () { return null; },
        querySelectorAll: function () { return []; },
        closest: function () { return null; },
        appendChild: function () {},
        removeChild: function () {},
        classList: {
            add: function (c) { classes.add(c); },
            remove: function (c) { classes.delete(c); },
            contains: function (c) { return classes.has(c); },
            toggle: function (c) { if (classes.has(c)) { classes.delete(c); } else { classes.add(c); } },
        },
    };
    Object.defineProperty(el, 'innerHTML', {
        get: function () { return this._html; },
        set: function (v) { this._html = String(v); },
    });
    return el;
}
function getEl(id) {
    if (!Object.prototype.hasOwnProperty.call(elements, id)) elements[id] = makeElement(id);
    return elements[id];
}
const selectorEls = {};
global.document = {
    getElementById: getEl,
    querySelector: function (sel) {
        if (!Object.prototype.hasOwnProperty.call(selectorEls, sel)) selectorEls[sel] = makeElement(sel);
        return selectorEls[sel];
    },
    querySelectorAll: function () { return []; },
    createElement: function (tag) { return makeElement('created-' + tag); },
    body: { appendChild: function () {}, removeChild: function () {} },
    addEventListener: function () {},
};

require('./data-refresh.js');
require('./image-backends-page.js');
const Page = global.ImageBackendsPage;
assert.ok(Page, 'window.ImageBackendsPage должен быть экспортирован');
const pure = Page.pure;

const sleep = function (ms) { return new Promise(function (r) { setTimeout(r, ms); }); };

// Phase 9: управление бэкендами живёт в табе «Обзор» объединённой страницы
// «Image-модели», поэтому и навигация, и страница — от неё (селектор берём
// буквально тот, что модуль ищет: a[data-page="image"]).
const NAV_SEL = 'a[data-page="image"]';
function navEl() { return selectorEls[NAV_SEL]; }
function pageEl() { return getEl('image-page'); }
function bodyHtml() { return getEl('imageBackendsBody').innerHTML; }

/** Синтетический клик по [data-ib-action] внутри делегированного контейнера. */
function clickAction(action, id) {
    const target = makeElement('click-target');
    target._attrs['data-ib-action'] = action;
    if (id !== undefined) target._attrs['data-ib-id'] = id;
    pageEl().dispatch('click', { target: target });
}

/** Заполнить поля формы редактора (модуль читает их через getElementById). */
function fillForm(fields) {
    Object.keys(fields).forEach(function (key) {
        const map = { id: 'ibId', name: 'ibName', host: 'ibHost', port: 'ibPort', gpu: 'ibGpu' };
        getEl(map[key]).value = fields[key];
    });
}

function lastRequest(filter) {
    for (let i = requests.length - 1; i >= 0; i--) {
        if (filter(requests[i])) return requests[i];
    }
    return null;
}

(async function run() {
    console.log('image-backends-page smoke');

    // --- 1. Статическая сверка JS <-> HTML <-> index.html -----------------
    await check('id страницы объявлены в webui/index.html', function () {
        // Phase 9: отдельной страницы «Image-бэкенды» больше нет — её карточка
        // живёт в табе «Обзор» объединённой страницы «Image-модели»
        // (id таблицы/политики/notice сохранены, поэтому модуль не менялся).
        const required = ['image-page', 'imTabOverview', 'imageBackendsTable', 'imageBackendsBody',
            'imageBackendsAdd', 'imageBackendsNotice'];
        const missing = required.filter(function (id) { return !HTML_IDS.has(id); });
        assert.deepStrictEqual(missing, [], 'нет в index.html: ' + missing.join(', '));
    });

    await check('все литеральные id модуля есть в index.html или генерируются модулем', function () {
        const missing = Array.from(LITERAL_IDS).filter(function (id) {
            return !HTML_IDS.has(id) && !GENERATED_IDS.has(id);
        });
        assert.deepStrictEqual(missing, [], 'id без разметки: ' + missing.join(', '));
        assert.ok(LITERAL_IDS.size >= 2, 'ожидались литеральные id, найдено ' + LITERAL_IDS.size);
    });

    await check('константы модуля: страница/таблица в index.html, модалка генерируется', function () {
        ['PAGE_ID', 'BODY_ID', 'NOTICE_ID'].forEach(function (name) {
            assert.ok(MODULE_CONSTS[name], 'нет константы ' + name);
            assert.ok(HTML_IDS.has(MODULE_CONSTS[name]), name + '=' + MODULE_CONSTS[name] + ' нет в index.html');
        });
        ['OVERLAY_ID', 'EDITOR_BODY_ID'].forEach(function (name) {
            assert.ok(GENERATED_IDS.has(MODULE_CONSTS[name]), name + '=' + MODULE_CONSTS[name] + ' не генерируется модулем');
        });
    });

    await check('разметка редактора: id полей уникальны, значения подставлены, теги закрыты', function () {
        ['ibEditorOverlay', 'ibEditorBody', 'ibEditorTitle', 'ibErrors'].forEach(function (id) {
            assert.ok(GENERATED_IDS.has(id), 'нет id ' + id + ' в разметке редактора');
        });
        // Поля формы рисует хелпер field('ibX', ...), поэтому литерала id="ibX"
        // в исходнике нет - сверяем имена полей с тем, что читает readForm()
        // (рассинхрон «поле нарисовано, но читается под другим id» тут и ловится).
        const fieldIds = new Set();
        for (const m of MODULE_SRC.matchAll(/field\('(ib[A-Za-z0-9]*)'/g)) fieldIds.add(m[1]);
        const readIds = new Set();
        for (const m of MODULE_SRC.matchAll(/value\('(ib[A-Za-z0-9]*)'\)/g)) readIds.add(m[1]);
        assert.ok(fieldIds.size >= 4, 'ожидались поля формы, найдено ' + fieldIds.size);
        const orphans = Array.from(readIds).filter(function (id) {
            return !fieldIds.has(id) && !GENERATED_IDS.has(id);
        });
        assert.deepStrictEqual(orphans, [], 'readForm читает поля, которых нет в разметке: ' + orphans.join(', '));

        // И фактическая разметка создания/редактирования.
        Page._actions.openEditor(null);
        const html = getEl('ibEditorBody').innerHTML;
        ['ibId', 'ibName', 'ibHost', 'ibPort', 'ibGpu', 'ibErrors'].forEach(function (id) {
            assert.ok(html.indexOf('id="' + id + '"') !== -1, 'нет id ' + id + ' в сгенерированной разметке');
        });
        // Подписи «Сосуществование с текстом: image_cpp» в редакторе быть НЕ должно:
        // это литерал типа бэкенда, который выглядел как значение настройки, а
        // фактическая политика показывается на самой странице (#imageBackendsPolicy).
        assert.strictEqual(html.indexOf('image_cpp'), -1, 'в редакторе снова появился литерал типа как «политика»');
        assert.ok(html.indexOf('undefined') === -1, 'в разметке есть undefined');
        assert.ok(/id="ibId"[^>]*readonly/.test(html) === false, 'при создании id должен быть редактируемым');
        const ids = Array.from(html.matchAll(/\bid="([A-Za-z0-9_-]+)"/g)).map(function (m) { return m[1]; });
        assert.deepStrictEqual(ids.filter(function (id, i) { return ids.indexOf(id) !== i; }), [], 'дубли id в разметке');

        // Редактирование: id только для чтения и подставлен из бэкенда.
        Page._actions.openEditor(pure.normalizeBackends(clusterBackends)[0]);
        const editHtml = getEl('ibEditorBody').innerHTML;
        assert.ok(/id="ibId"[^>]*value="imageworker"[^>]*readonly/.test(editHtml), 'id не заполнен/не readonly при редактировании');
        assert.ok(/id="ibPort"[^>]*value="18093"/.test(editHtml), 'порт не подставлен: ' + editHtml.slice(0, 400));
        assert.ok(/id="ibGpu"[^>]*>/.test(editHtml) && editHtml.indexOf('<option value="0" selected>') !== -1,
            'GPU-индекс 0 не выбран в списке');
        assert.ok(editHtml.indexOf('id="ibErrors"') !== -1, 'нет контейнера ошибок');
        Page._actions.closeEditor();
    });

    await check('пункт навигации и страница «Image-модели» есть в index.html, модуль подключён', function () {
        // Phase 9: управление image-бэкендами переехало в таб «Обзор» страницы
        // «Image-модели» — отдельного пункта меню нет, а видимость страницы
        // решает syncVisibility по составу кластера (проверяется ниже).
        assert.ok(/<a href="#image" class="nav-item" data-page="image">/.test(INDEX_HTML),
            'нет пункта навигации <a data-page="image">');
        assert.ok(/id="image-page"/.test(INDEX_HTML), 'нет страницы id="image-page"');
        assert.ok(INDEX_HTML.indexOf('id="imTabOverview"') !== -1, 'нет таба «Обзор»');
        assert.ok(INDEX_HTML.indexOf('data-i18n="nav.image_models"') !== -1, 'нет data-i18n пункта');
        assert.ok(INDEX_HTML.indexOf('js/modules/image-backends-page.js?v=R83') !== -1, 'нет script-тега модуля');
    });

    await check('app.js: таблица рендерится на странице «Image-модели» и видимость синхронизируется', function () {
        // Phase 9: собственного case у страницы бэкендов больше нет — рендер и
        // видимость висят на case 'image' (таб «Обзор»), а периодический опрос
        // кластера обновляет таблицу, пока страница открыта.
        assert.ok(/case 'image':/.test(APP_SRC), "нет case 'image' в refreshPage");
        assert.ok(APP_SRC.indexOf('window.ImageBackendsPage.render(') !== -1, 'app.js не рендерит таблицу');
        assert.ok(APP_SRC.indexOf('window.ImageBackendsPage.syncVisibility(') !== -1, 'app.js не синхронизирует видимость');
        assert.ok(APP_SRC.indexOf("page === 'image'") !== -1, 'switchPage не перечитывает кластер для страницы');
        assert.ok(APP_SRC.indexOf("currentPage === 'image'") !== -1, 'периодический опрос не обновляет таблицу');
        assert.strictEqual(/case 'image-backends':/.test(APP_SRC), false, 'остался мёртвый case image-backends');
    });

    // --- 2. mount + рендер таблицы ---------------------------------------
    await check('mount() находит страницу и подписывает делегированный клик', function () {
        assert.strictEqual(Page.mount(), true);
        assert.ok(pageEl()._listeners['click'] && pageEl()._listeners['click'].length === 1,
            'делегированный обработчик подписан ровно один раз');
        // Повторный mount не должен дублировать обработчики.
        Page.mount();
        assert.strictEqual(pageEl()._listeners['click'].length, 1, 'mount() не идемпотентен');
    });

    const normalized = Page.render(clusterBackends);
    await check('render() отбирает только image_cpp и нормализует поля воркера', function () {
        assert.strictEqual(normalized.length, 1, 'llama_cpp не должен попадать на страницу');
        const b = normalized[0];
        assert.strictEqual(b.id, 'imageworker');
        assert.strictEqual(b.port, 18093);
        assert.strictEqual(b.gpuIndex, 0, 'GPU 0 - валидный индекс, не «не задан»');
        assert.strictEqual(b.state, 'loaded');
        assert.strictEqual(b.currentModel, 'sd15-q4');
        assert.strictEqual(b.requests.total, 7);
        assert.strictEqual(b.requests.inFlight, 0);
        assert.strictEqual(b.requests.avgDurationMs, 9100);
    });

    await check('таблица: порт воркера, состояние, модель, VRAM и счётчики запросов', function () {
        const html = bodyHtml();
        assert.ok(html.indexOf('imageworker') !== -1, 'нет id бэкенда');
        assert.ok(html.indexOf('>18093<') !== -1, 'нет порта воркера: ' + html.slice(0, 400));
        assert.ok(html.indexOf('>0<') !== -1, 'нет GPU-индекса');
        assert.strictEqual(html.indexOf(RU['imageBackends.gpu_unknown']), -1,
            'GPU 0 показан как «не задан» (0 - валидная первая карта)');
        assert.ok(html.indexOf('Загружена') !== -1, 'нет локализованного состояния воркера');
        assert.ok(html.indexOf('sd15-q4') !== -1, 'нет текущей модели');
        assert.ok(html.indexOf('894 MB / 8.0 GB') !== -1, 'нет строки VRAM: ' + html.slice(0, 400));
        assert.ok(html.indexOf('data-ib-req="total"') !== -1, 'нет счётчика total');
        assert.ok(/data-ib-req="total"[^>]*>7</.test(html), 'total != 7');
        assert.ok(/data-ib-req="ok"[^>]*>6</.test(html), 'ok != 6');
        assert.ok(/data-ib-req="failed"[^>]*>1</.test(html), 'failed != 1');
        assert.ok(/data-ib-req="rejected"[^>]*>2</.test(html), 'rejected != 2');
        assert.ok(/data-ib-req="inflight"[^>]*>0</.test(html), 'inflight != 0');
        assert.ok(html.indexOf('title="Всего"') !== -1, 'подписи счётчиков не локализованы');
        assert.ok(html.indexOf('>0.10<') !== -1, 'нет RPS');
        assert.ok(html.indexOf('>9.1 s<') !== -1, 'нет среднего времени: ' + html.slice(0, 600));
        // llama_cpp-бэкенд на странице image-бэкендов не рисуется.
        assert.strictEqual(html.indexOf('llama-1'), -1, 'чужой тип бэкенда попал в таблицу');
    });

    await check('таблица: кнопки действий для каждой строки', function () {
        const html = bodyHtml();
        ['edit', 'remove', 'open-images'].forEach(function (a) {
            assert.ok(html.indexOf('data-ib-action="' + a + '"') !== -1, 'нет кнопки ' + a);
        });
        assert.ok(/data-ib-action="remove" data-ib-id="imageworker"/.test(html), 'кнопки не привязаны к id бэкенда');
        // У движка одна модель на процесс, поэтому показывается РОВНО одна кнопка
        // управления моделью. Загружена -> «Выгрузить», «Загрузить» не предлагается.
        assert.ok(/data-ib-action="unload" data-ib-id="imageworker">/.test(html), 'нет активной кнопки «Выгрузить»');
        assert.strictEqual(html.indexOf('data-ib-action="load"'), -1, 'у загруженной модели предложена кнопка «Загрузить»');
    });

    await check('таблица: у незагруженной модели предложена «Загрузить», а не «Выгрузить»', function () {
        const notLoaded = clusterPayload().backends;
        notLoaded[0].image.state = 'not_loaded';
        const normalized = Page.render(notLoaded);
        assert.strictEqual(normalized.length, 1);
        const html = bodyHtml();
        assert.ok(html.indexOf('data-ib-action="load"') !== -1, 'нет кнопки «Загрузить» для незагруженной модели');
        assert.strictEqual(html.indexOf('data-ib-action="unload"'), -1, 'предложена «Выгрузить» при незагруженной модели');
    });

    await check('таблица: состояние loading -> «Загрузить» disabled (движок занят)', function () {
        const loading = clusterPayload().backends;
        loading[0].image.state = 'loading';
        Page.render(loading);
        const html = bodyHtml();
        assert.ok(/data-ib-action="load"[^>]*disabled/.test(html), 'во время загрузки кнопка «Загрузить» должна быть disabled');
    });

    // Политика сосуществования и лимиты гейта - глобальные (balancing.image), а не
    // свойство бэкенда. Раньше на странице стоял литерал «...: image_cpp», который
    // выглядел как значение настройки; теперь показывается ФАКТИЧЕСКАЯ политика из
    // /api/v1/metrics (coexistence_policy + лимиты).
    await check('политика сосуществования берётся из /api/v1/metrics', async function () {
        Page.render(clusterPayload().backends);
        await new Promise(function (r) { setTimeout(r, 0); });
        await new Promise(function (r) { setTimeout(r, 0); });
        const txt = getEl('imageBackendsPolicy').textContent;
        assert.ok(txt.indexOf('exclusive') !== -1, 'нет политики из метрик: ' + txt);
        assert.ok(txt.indexOf('512 MB') !== -1, 'нет vram headroom: ' + txt);
        assert.ok(txt.indexOf('wait 60s') !== -1, 'нет ожидания очереди: ' + txt);
        assert.ok(txt.indexOf('fuse 120s') !== -1, 'нет предохранителя лока: ' + txt);
        assert.strictEqual(txt.indexOf('image_cpp'), -1, 'в политику попал literal типа бэкенда');
        // Запрос ушёл именно в метрики (в /api/v1/cluster этих полей нет).
        const metricsReq = requests.filter(function (r) { return r.path === '/api/v1/metrics'; });
        assert.ok(metricsReq.length >= 1, 'страница не запросила /api/v1/metrics');
    });

    await check('render() терпит бэкенд без блока image (воркер не отчитался метриками)', function () {
        const fresh = pure.normalizeBackends([{ id: 'image-new', type: 'image_cpp', host: '10.0.0.9', imagePort: 18093 }]);
        assert.strictEqual(fresh.length, 1);
        assert.strictEqual(fresh[0].state, '');
        assert.strictEqual(fresh[0].gpuIndex, null, 'отсутствующий gpuIndex = «неизвестно»');
        assert.strictEqual(fresh[0].requests.total, 0);
        Page.render([{ id: 'image-new', type: 'image_cpp', host: '10.0.0.9', imagePort: 18093 }]);
        const html = bodyHtml();
        assert.ok(html.indexOf('image-new') !== -1, 'строка не отрисована');
        assert.ok(html.indexOf(RU['imageBackends.gpu_unknown']) !== -1, 'нет текста «не задан» для неизвестного GPU');
        assert.ok(html.indexOf('>18093<') !== -1, 'нет порта воркера');
        assert.ok(html.indexOf('undefined') === -1, 'в разметке есть undefined');
    });

    await check('тип бэкенда читается и из type (/api/v1/backends), и из backendType (/api/v1/cluster)', function () {
        assert.strictEqual(pure.isImageBackend({ type: 'image_cpp' }), true);
        assert.strictEqual(pure.isImageBackend({ backendType: 'image_cpp' }), true);
        assert.strictEqual(pure.isImageBackend({ backend_type: 'image_cpp' }), true);
        assert.strictEqual(pure.isImageBackend({ backendType: 'llama_cpp' }), false);
        assert.strictEqual(pure.isImageBackend({}), false);
    });

    // --- 3. Пустой список -------------------------------------------------
    await check('пустой список рендерит imageBackends.empty', function () {
        Page.render([]);
        assert.ok(bodyHtml().indexOf(RU['imageBackends.empty']) !== -1, 'нет текста пустого состояния: ' + bodyHtml());
        Page.render(clusterBackends);
    });

    // --- 4. syncVisibility ------------------------------------------------
    await check('syncVisibility: при наличии image_cpp пункт и страница видимы', function () {
        const visible = Page.syncVisibility(clusterBackends);
        assert.strictEqual(visible, true);
        assert.strictEqual(navEl().style.display, '');
        assert.strictEqual(pageEl().style.display, '');
    });

    await check('syncVisibility: без image-бэкендов пункт и страница скрыты', function () {
        const visible = Page.syncVisibility([{ id: 'llama-1', backendType: 'llama_cpp' }]);
        assert.strictEqual(visible, false);
        assert.strictEqual(navEl().style.display, 'none');
        assert.strictEqual(pageEl().style.display, 'none');
        const empty = Page.syncVisibility([]);
        assert.strictEqual(empty, false);
        assert.strictEqual(navEl().style.display, 'none');
    });

    await check('syncVisibility: image-бэкенд без метрик всё равно показывает пункт', function () {
        assert.strictEqual(Page.syncVisibility([{ id: 'image-x', backendType: 'image_cpp' }]), true);
        assert.strictEqual(navEl().style.display, '');
    });

    await check('hasImageBackends не считает чужие типы', function () {
        assert.strictEqual(pure.hasImageBackends([{ type: 'ollama' }, { type: 'llama_cpp' }]), false);
        assert.strictEqual(pure.hasImageBackends([{ type: 'ollama' }, { type: 'image_cpp' }]), true);
        assert.strictEqual(pure.hasImageBackends(null), false);
    });

    // --- 5. Валидация формы ----------------------------------------------
    await check('validateForm: пустой id/host и битый порт дают коды i18n-ошибок', function () {
        const bad = pure.validateForm({ id: '', host: '', port: 'abc' });
        assert.strictEqual(bad.ok, false);
        assert.deepStrictEqual(bad.errors.map(function (e) { return e.key; }), [
            'imageBackends.err.id_required',
            'imageBackends.err.host_required',
            'imageBackends.err.port_required',
        ]);
        // Порядок 1..65535: 0, 70000 и отрицательное - невалидны.
        ['0', '70000', '-1', '18093abc', ''].forEach(function (port) {
            const res = pure.validateForm({ id: 'x', host: 'h', port: port });
            assert.strictEqual(res.ok, false, 'порт "' + port + '" должен быть отвергнут');
            assert.strictEqual(res.errors[0].key, 'imageBackends.err.port_required');
        });
        assert.strictEqual(pure.validateForm({ id: 'x', host: 'h', port: '18093' }).ok, true);
        assert.strictEqual(pure.validateForm({ id: 'x', host: 'h', port: '1' }).ok, true);
        assert.strictEqual(pure.validateForm({ id: 'x', host: 'h', port: '65535' }).ok, true);
    });

    await check('форма добавления: пустой id -> ошибка на экране, запрос НЕ уходит', async function () {
        Page.mount();
        Page._actions.openEditor(null);
        fillForm({ id: '', name: 'no-id', host: 'imageworker', port: '18093', gpu: '' });
        const before = requests.length;
        const res = await Page._actions.save();
        assert.strictEqual(res.ok, false);
        const posts = requests.slice(before).filter(function (r) { return r.method === 'POST' || r.method === 'PUT'; });
        assert.deepStrictEqual(posts, [], 'невалидная форма не должна уходить на сервер');
        assert.strictEqual(getEl('ibErrors').innerHTML, RU['imageBackends.err.id_required'],
            'в форме нет текста ошибки: ' + getEl('ibErrors').innerHTML);
        assert.strictEqual(getEl('imageBackendsNotice').innerHTML, RU['imageBackends.err.id_required']);
        assert.strictEqual(getEl('imageBackendsNotice').style.display, '');
        const last = toasts[toasts.length - 1];
        assert.strictEqual(last.type, 'error');
        assert.ok(last.msg.indexOf('imageBackends.err.') === -1, 'показан сырой i18n-ключ: ' + last.msg);
    });

    await check('форма добавления: пустой host и битый порт -> ожидаемые тексты', async function () {
        Page._actions.openEditor(null);
        fillForm({ id: 'img2', name: 'img2', host: '', port: '18093', gpu: '' });
        await Page._actions.save();
        let text = getEl('ibErrors').innerHTML;
        assert.ok(text.indexOf(RU['imageBackends.err.host_required']) !== -1, 'нет ошибки хоста: ' + text);

        fillForm({ id: 'img2', name: 'img2', host: 'imageworker', port: '99999', gpu: '' });
        await Page._actions.save();
        text = getEl('ibErrors').innerHTML;
        assert.ok(text.indexOf(RU['imageBackends.err.port_required']) !== -1, 'нет ошибки порта: ' + text);

        // Обе ошибки показываются сразу, а не только первая.
        fillForm({ id: '', name: '', host: '', port: 'x', gpu: '' });
        await Page._actions.save();
        text = getEl('ibErrors').innerHTML;
        assert.ok(text.indexOf(RU['imageBackends.err.id_required']) !== -1, 'нет ошибки id');
        assert.ok(text.indexOf(RU['imageBackends.err.host_required']) !== -1, 'нет ошибки хоста');
        assert.ok(text.indexOf(RU['imageBackends.err.port_required']) !== -1, 'нет ошибки порта');
    });

    await check('форма добавления: валидная форма -> POST с backendType=image_cpp, gpuIndex только когда задан', async function () {
        Page._actions.openEditor(null);
        fillForm({ id: 'img2', name: 'gpu image 2', host: '10.0.0.9', port: '18100', gpu: '' });
        const before = requests.length;
        const res = await Page._actions.save();
        assert.strictEqual(res.ok, true, 'сохранение должно пройти');
        const post = requests.slice(before).filter(function (r) { return r.path === '/api/v1/backends' && r.method === 'POST'; })[0];
        assert.ok(post, 'POST /api/v1/backends не ушёл');
        assert.strictEqual(post.headers['X-API-Token'], 'test-token', 'нет заголовка авторизации');
        assert.deepStrictEqual(post.body, {
            id: 'img2', name: 'gpu image 2', host: '10.0.0.9', imagePort: 18100, backendType: 'image_cpp',
        });
        assert.strictEqual('gpuIndex' in post.body, false, 'при пустом GPU ключ gpuIndex не шлём');
        assert.ok(toasts.filter(function (t) { return t.msg === RU['imageBackends.saved']; }).length >= 1, 'нет тоста сохранения');

        // Явный индекс 0 (первая карта) обязан уйти числом.
        Page._actions.openEditor(null);
        fillForm({ id: 'img3', name: 'img3', host: '10.0.0.9', port: '18101', gpu: '0' });
        const before2 = requests.length;
        await Page._actions.save();
        const post2 = requests.slice(before2).filter(function (r) { return r.path === '/api/v1/backends' && r.method === 'POST'; })[0];
        assert.ok(post2, 'POST не ушёл');
        assert.strictEqual(post2.body.gpuIndex, 0, 'GPU 0 должен уйти числом, а не отсутствовать');
        Page._actions.closeEditor();
    });

    await check('редактирование: PUT с gpuIndex=null, когда поле очищено', async function () {
        Page.render(clusterBackends);
        assert.strictEqual(Page._actions.openEditor(pure.normalizeBackends(clusterBackends)[0]), true);
        fillForm({ id: 'imageworker', name: 'imageworker', host: 'imageworker', port: '18093', gpu: '' });
        const before = requests.length;
        const res = await Page._actions.save();
        assert.strictEqual(res.ok, true, 'PUT должен пройти');
        const put = requests.slice(before).filter(function (r) {
            return r.path === '/api/v1/backends/imageworker' && r.method === 'PUT';
        })[0];
        assert.ok(put, 'PUT /api/v1/backends/imageworker не ушёл');
        assert.strictEqual(put.body.gpuIndex, null, 'очищенное поле = сброс в «неизвестно» (null)');
        assert.strictEqual(put.body.backendType, 'image_cpp');
        assert.strictEqual(put.body.imagePort, 18093);
        Page._actions.closeEditor();
    });

    // --- 6. Удаление ------------------------------------------------------
    await check('кнопка «Удалить» -> confirm -> DELETE /api/v1/backends/{id} + перезагрузка', async function () {
        Page.render(clusterBackends);
        confirmAnswer = true;
        const before = requests.length;
        clickAction('remove', 'imageworker');
        await sleep(20);
        const del = requests.slice(before).filter(function (r) {
            return r.path === '/api/v1/backends/imageworker' && r.method === 'DELETE';
        })[0];
        assert.ok(del, 'DELETE не ушёл');
        assert.strictEqual(del.headers['X-API-Token'], 'test-token');
        assert.ok(requests.slice(before).filter(function (r) { return r.path === '/api/v1/cluster'; }).length >= 1,
            'после удаления список не перечитан');
        assert.ok(toasts.filter(function (t) { return t.msg === RU['imageBackends.removed']; }).length >= 1, 'нет тоста удаления');
    });

    await check('кнопка «Удалить»: отказ в confirm -> запроса нет', async function () {
        confirmAnswer = false;
        const before = requests.length;
        clickAction('remove', 'imageworker');
        await sleep(20);
        assert.deepStrictEqual(requests.slice(before).filter(function (r) { return r.method === 'DELETE'; }), [],
            'DELETE не должен уходить без подтверждения');
        confirmAnswer = true;
    });

    // --- 7. Модели воркера ------------------------------------------------
    await check('«Загрузить модель» -> POST .../models/load с текущей моделью', async function () {
        Page.render(clusterBackends);
        const before = requests.length;
        clickAction('load', 'imageworker');
        await sleep(20);
        const post = requests.slice(before).filter(function (r) {
            return r.path === '/api/v1/image/backends/imageworker/models/load';
        })[0];
        assert.ok(post, 'POST load не ушёл');
        assert.strictEqual(post.method, 'POST');
        assert.deepStrictEqual(post.body, { name: 'sd15-q4' });
    });

    await check('«Выгрузить модель» -> POST .../models/unload', async function () {
        Page.render(clusterBackends);
        const before = requests.length;
        clickAction('unload', 'imageworker');
        await sleep(20);
        const post = requests.slice(before).filter(function (r) {
            return r.path === '/api/v1/image/backends/imageworker/models/unload';
        })[0];
        assert.ok(post, 'POST unload не ушёл');
        assert.deepStrictEqual(post.body, { name: 'sd15-q4' });
    });

    await check('неизвестная текущая модель: GET .../models -> prompt -> POST load', async function () {
        const noModel = [{ id: 'image-blind', backendType: 'image_cpp', host: 'h', imagePort: 18093, image: { state: 'not_loaded' } }];
        Page.render(noModel);
        promptAnswer = 'sdxl-base';
        const before = requests.length;
        clickAction('load', 'image-blind');
        await sleep(20);
        const list = requests.slice(before).filter(function (r) {
            return r.path === '/api/v1/image/backends/image-blind/models' && r.method === 'GET';
        })[0];
        assert.ok(list, 'GET списка моделей воркера не ушёл');
        const post = lastRequest(function (r) { return r.path === '/api/v1/image/backends/image-blind/models/load'; });
        assert.ok(post, 'POST load не ушёл');
        assert.deepStrictEqual(post.body, { name: 'sdxl-base' });
        promptAnswer = '';
        Page.render(clusterBackends);
    });

    await check('«К генерации» -> клик по пункту навигации страницы Image', function () {
        const imageNav = global.document.querySelector('[data-page="image"]');
        const before = imageNav._clicks;
        clickAction('open-images', 'imageworker');
        assert.strictEqual(imageNav._clicks, before + 1, 'переключения на страницу Image не было');
    });

    // --- 8. Навигация и заголовок ----------------------------------------
    // R84 (2026-10-03): кнопки «Обновить» у страницы нет — её заменил индикатор
    // свежести в шапке: он дёргает провайдера страницы 'image'. Поэтому проверяем
    // не клик по кнопке, а сам провайдер.
    await check('провайдер обновления страницы перечитывает /api/v1/cluster и перерисовывает таблицу', async function () {
        clusterBackends = clusterPayload().backends;
        const before = requests.filter(function (r) { return r.path === '/api/v1/cluster'; }).length;
        assert.ok(global.DataRefresh, 'data-refresh.js должен быть загружен (провайдеры страниц)');
        assert.ok(global.DataRefresh._providers['image'] && global.DataRefresh._providers['image'].length > 0,
            'страница Image не зарегистрировала провайдера обновления');
        const ok = await global.DataRefresh.refresh('image');
        assert.strictEqual(ok, true, 'провайдер вернул ошибку');
        assert.ok(requests.filter(function (r) { return r.path === '/api/v1/cluster'; }).length > before,
            'нет запроса кластера через провайдера страницы');
        assert.ok(bodyHtml().indexOf('imageworker') !== -1, 'таблица не перерисована');
    });

    await check('render() при ошибке кластера не роняет страницу (notice с текстом)', async function () {
        const realFetch = global.fetch;
        global.fetch = function () { return Promise.reject(new Error('network down')); };
        const list = await Page.refresh();
        assert.deepStrictEqual(list, []);
        assert.strictEqual(getEl('imageBackendsNotice').style.display, '');
        assert.ok(getEl('imageBackendsNotice').innerHTML.indexOf('network down') !== -1,
            'текст ошибки не показан: ' + getEl('imageBackendsNotice').innerHTML);
        global.fetch = realFetch;
    });

    await check('смена языка перерисовывает динамические строки (i18n:changed)', function () {
        Page.render(clusterBackends);
        assert.ok(i18nListeners.length >= 1, 'модуль не подписан на i18n:changed');
        const before = bodyHtml();
        getEl('imageBackendsBody').innerHTML = 'stale';
        i18nListeners.forEach(function (fn) { fn({}); });
        assert.notStrictEqual(bodyHtml(), 'stale', 'строки не перерисованы после смены языка');
        assert.strictEqual(bodyHtml(), before, 'перерисовка дала другой результат');
    });

    // --- 9. i18n ----------------------------------------------------------
    await check('все i18n-ключи, которые дёргает модуль, есть в ru.js', function () {
        assert.deepStrictEqual(missingKeys, [], 'нет ключей в ru.js: ' + missingKeys.join(', '));
    });

    await check('data-i18n ключи страницы в index.html есть в ru.js', function () {
        const keys = Array.from(INDEX_HTML.matchAll(/data-i18n="((?:imageBackends|nav)\.[A-Za-z0-9_.]+)"/g)).map(function (m) { return m[1]; });
        assert.ok(keys.length >= 20, 'ожидались ключи страницы, найдено ' + keys.length);
        const missing = keys.filter(function (k) { return RU[k] === undefined; });
        assert.deepStrictEqual(missing, [], 'нет в ru.js: ' + missing.join(', '));
    });

    // Живой дефект (2026-10-08): таблица «Управление image-бэкендами» показывала
    // «не задан» в GPU, «-» в состоянии и модели и «0 / 0» в VRAM — у ВСЕХ строк,
    // включая живой бэкенд с реальными метриками. Причина: страница берёт данные
    // из GET /api/v1/cluster, где блок `image` появляется только после первого
    // снапшота поллера ресурсов. Пока его нет, всё читалось из пустого объекта.
    // Те же сведения в кластере лежат в других полях — проверяем запасной путь.
    await check('без блока image строка заполняется из полей /api/v1/cluster', function () {
        const row = pure.normalizeBackends([{
            id: 'imageworker', backendType: 'image_cpp', status: 'healthy', host: 'imageworker',
            imagePort: 18093, maxConcurrentRequests: 64,
            gpu: { memoryTotal: 8192, memoryUsed: 2023, memoryFree: 5995, temperature: 57, uuids: ['GPU-6f8d'] },
            models: [{ name: 'qwen-image-2.1-uncensored-gguf', state: 'loaded' }],
            ollama: { activeRequests: 0, totalRequests: 5, requestsPerSecond: 0.2, avgResponseTime: 9100, runningModels: null },
        }]);

        assert.strictEqual(row.length, 1, 'image-бэкенд должен попасть в таблицу');
        const b = row[0];
        assert.strictEqual(b.state, 'healthy', 'состояние должно браться из status');
        assert.strictEqual(b.currentModel, 'qwen-image-2.1-uncensored-gguf', 'модель должна браться из models');
        assert.strictEqual(b.vramFreeMb, 5995, 'VRAM free должен браться из gpu.memoryFree');
        assert.strictEqual(b.vramTotalMb, 8192, 'VRAM total должен браться из gpu.memoryTotal');
        assert.strictEqual(b.requests.total, 5, 'запросы должны браться из ollama.totalRequests');
        assert.strictEqual(b.requests.avgDurationMs, 9100, 'среднее время должно браться из ollama.avgResponseTime');
        assert.strictEqual(pure.formatVramPair(b.vramFreeMb, b.vramTotalMb), '5.9 GB / 8.0 GB',
            'VRAM в таблице должна быть заполнена, а не «0 / 0»');
    });

    // Приоритет остаётся у блока image, когда он есть: он точнее (знает состояние
    // воркера и его собственные счётчики запросов).
    await check('блок image имеет приоритет над полями кластера', function () {
        const row = pure.normalizeBackends([{
            id: 'imageworker', backendType: 'image_cpp', status: 'healthy', host: 'imageworker',
            gpu: { memoryTotal: 8192, memoryFree: 1 },
            models: [{ name: 'from-cluster' }],
            ollama: { totalRequests: 999 },
            image: { state: 'loaded', currentModel: 'from-image', vramFreeMb: 894, vramTotalMb: 8192, requests: { total: 7 } },
        }]);
        assert.strictEqual(row[0].state, 'loaded', 'состояние должно остаться из image');
        assert.strictEqual(row[0].currentModel, 'from-image');
        assert.strictEqual(row[0].vramFreeMb, 894);
        assert.strictEqual(row[0].requests.total, 7);
    });

    console.log('\n' + (failures.length ? 'FAILED: ' + failures.length : 'OK: ' + passed + ' checks passed'));
    if (failures.length) {
        failures.forEach(function (f) { console.error(' - ' + f); });
        process.exit(1);
    }
})();
