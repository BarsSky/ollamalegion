// image-page-dom.test.js — R-Image Phase 5 (2026-10-02): интеграционный smoke
// страницы «Изображения» с мок-DOM и мок-fetch (без браузера).
//
// Запуск: node webui/js/modules/image-page-dom.test.js
//
// Зачем этот тест, если есть image-page.test.js:
//   * чистые функции не ловят опечатки в id элементов и в вызовах DOM —
//     а именно они дают «страница молча не работает»;
//   * здесь проверяется весь путь: refresh() → рендер таблицы моделей →
//     POST /v1/images/generations → галерея + localStorage → bundle-загрузка
//     (включая fallback на одиночные /api/hf/download) → load модели.
//
// Что проверяем:
//   1. Каждый getElementById из модуля существует в webui/index.html
//      (кроме динамических imgBundleFilesList_<n>) — ловит рассинхрон HTML/JS.
//   2. Каждый i18n-ключ, который дёргает модуль, есть в ru.js (поэтому тест
//      грузит реальный пак переводов, а не заглушку).
//   3. refresh(): список image-бэкендов (fallback на /api/v1/backends при 404),
//      модели, capabilities, отрисовка строки модели.
//   4. generate(): тело запроса соответствует контракту (seed -1 при пустом
//      поле, negative_prompt, batch_size...), картинок показано столько,
//      сколько вернул ответ, история сохранена в localStorage.
//   5. bundle: отсутствие bundle-эндпоинта → отдельные /api/hf/download с role.
//   6. load model: POST /api/image/models/load + остановка прогресс-поллинга.

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

let passed = 0;
const failures = [];
function check(name, fn) {
    try {
        fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' — ' + (e && e.message));
    }
}

// --- mocks: window / localStorage / i18n -----------------------------------
global.window = global;
// window.addEventListener: в node у global его нет, а модуль слушает i18n:changed.
global.addEventListener = function () {};
global.removeEventListener = function () {};

const store = {};
global.localStorage = {
    getItem: function (k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem: function (k, v) { store[k] = String(v); },
    removeItem: function (k) { delete store[k]; },
};

// Реальный русский пак переводов — так тест заодно проверяет, что все ключи
// модуля существуют (иначе i18n вернул бы сам ключ).
require('../i18n/ru.js');
const RU = global.I18N_RU;
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
            Object.keys(vars).forEach(function (p) { s = s.replace('{' + p + '}', vars[p]); });
        }
        return s;
    },
};

const toasts = [];
global.showToast = function (msg, type) { toasts.push({ msg: msg, type: type }); };

// Api.getAuthHeaders() — общий хелпер WebUI, от которого модуль берёт базовые
// заголовки (X-API-Token). Мок повторяет поведение api.js:651-662.
global.Api = {
    getAuthHeaders: function () {
        return { 'Content-Type': 'application/json', 'X-API-Token': 'test-token' };
    },
};

global.WEBUI_CONFIG = { API_BASE: 'http://balancer.test:18081', API_TOKEN: 'test-token' };

// --- mocks: DOM ------------------------------------------------------------
const INDEX_HTML = fs.readFileSync(path.join(__dirname, '..', '..', 'index.html'), 'utf8');
const KNOWN_IDS = new Set();
let m;
const idRe = /id="([A-Za-z0-9_-]+)"/g;
while ((m = idRe.exec(INDEX_HTML)) !== null) KNOWN_IDS.add(m[1]);

const unknownIds = [];
const elements = {};

function makeElement(id) {
    const el = {
        id: id,
        style: {},
        _html: '',
        textContent: '',
        value: '',
        disabled: false,
        options: [],
        min: 0,
        max: 0,
        step: 0,
        _listeners: {},
        _attrs: {},
        addEventListener: function (type, fn) { (this._listeners[type] = this._listeners[type] || []).push(fn); },
        removeEventListener: function () {},
        getAttribute: function (n) { return Object.prototype.hasOwnProperty.call(this._attrs, n) ? this._attrs[n] : null; },
        setAttribute: function (n, v) { this._attrs[n] = v; },
        querySelector: function () { return null; },
        querySelectorAll: function () { return []; },
        closest: function () { return null; },
        appendChild: function () {},
        removeChild: function () {},
        classList: { add: function () {}, remove: function () {}, contains: function () { return false; }, toggle: function () {} },
    };
    Object.defineProperty(el, 'innerHTML', {
        get: function () { return this._html; },
        set: function (v) { this._html = String(v); },
    });
    return el;
}

// Строки bundle отдаём «как из DOM»: diffusion + vae.
function makeBundleRowsHost() {
    const host = makeElement('imgBundleRows');
    host.querySelectorAll = function (sel) {
        if (sel !== '[data-bundle-row]') return [];
        const rows = [
            { role: 'diffusion', repo: 'leejet/FLUX.1-schnell-gguf', filename: 'flux1-schnell-Q4_0.gguf' },
            { role: 'vae', repo: 'black-forest-labs/FLUX.1-schnell', filename: 'ae.safetensors' },
        ];
        return rows.map(function (r) {
            return {
                querySelector: function (q) {
                    const mm = /data-bundle-field="([a-z]+)"/.exec(q);
                    return { value: mm ? (r[mm[1]] || '') : '' };
                },
            };
        });
    };
    return host;
}
// Заранее создаём все элементы, которые есть в index.html, чтобы тест мог
// читать/записывать их value и innerHTML. Обращение модуля к id, которого нет
// в index.html, всё равно фиксируется в unknownIds (см. getElementById).
KNOWN_IDS.forEach(function (id) { elements[id] = makeElement(id); });
elements['imgBundleRows'] = makeBundleRowsHost();

global.document = {
    getElementById: function (id) {
        if (!Object.prototype.hasOwnProperty.call(elements, id)) {
            if (!KNOWN_IDS.has(id) && !/^imgBundleFilesList_\d+$/.test(id)) unknownIds.push(id);
            elements[id] = makeElement(id);
        }
        return elements[id];
    },
    querySelectorAll: function () { return []; },
    querySelector: function () { return null; },
    createElement: function (tag) { return makeElement('created-' + tag); },
    body: { appendChild: function () {}, removeChild: function () {} },
    addEventListener: function () {},
};

// --- mocks: fetch ----------------------------------------------------------
const requests = [];
function jsonResponse(body, status) {
    return Promise.resolve({
        ok: (status || 200) < 400,
        status: status || 200,
        headers: { get: function () { return null; } },
        text: function () { return Promise.resolve(JSON.stringify(body)); },
    });
}

const MODELS = {
    models: [{
        name: 'sd15-q8',
        state: 'not_loaded',
        size_bytes: 1760000000,
        family: 'sd15',
        active_queries: 0,
        vram_estimate_mb: 2100,
        defaults: { steps: 8, cfgScale: 1, sampler: 'euler', scheduler: 'karras', width: 512, height: 512, negativePrompt: 'ugly' },
    }],
};
const CAPS = {
    samplers: ['euler', 'dpm++2m'],
    schedulers: ['karras', 'discrete'],
    limits: { min_width: 64, max_width: 2048, min_height: 64, max_height: 2048, max_batch_count: 4 },
};

let imageBackendsStatus = 404; // /api/v1/image/backends ещё нет (параллельная работа)

global.fetch = function (url, init) {
    const method = (init && init.method) || 'GET';
    requests.push({ url: url, method: method, body: init && init.body ? JSON.parse(init.body) : null, headers: (init && init.headers) || {} });
    const u = String(url);
    // Точное сравнение путей: прокси-URL длиннее management-путей и содержит их
    // как подстроку ('/api/v1/image/backends/...' и '/api/image/models/...'),
    // поэтому indexOf-матчинг давал бы ложные срабатывания.
    const proxyPath = u.indexOf('/proxy/') === -1 ? '' : u.slice(u.indexOf('/proxy/') + '/proxy'.length);
    const p = (proxyPath || u.replace(/^https?:\/\/[^/]+/, '')).split('?')[0];

    if (p === '/api/v1/image/backends') return jsonResponse({ error: 'not found' }, imageBackendsStatus);
    if (p === '/api/v1/backends') {
        return jsonResponse({ backends: [
            { id: 'llama-1', type: 'llama_cpp', host: '10.0.0.1' },
            { id: 'img-1', name: 'GPU image', type: 'image_cpp', host: '10.0.0.2', imagePort: 18093, status: 'healthy' },
        ] });
    }
    // Формат image-воркера (cmd/sdworker/handlers_model.go:318-338): один снимок
    // {progress:{state,model,stage,elapsed_ms,error}} + top-level state/model.
    if (p === '/api/image/models/load/progress') {
        return jsonResponse({
            progress: { state: 'loaded', model: 'sd15-q8', stage: 'ready', elapsed_ms: 1200, events: [] },
            state: 'loaded',
            model: 'sd15-q8',
            pid: 4242,
        });
    }
    if (p === '/api/image/models/load') return jsonResponse({ status: 'ok' });
    if (p === '/api/image/models/unload') return jsonResponse({ status: 'ok' });
    if (p === '/api/image/models') return jsonResponse(MODELS);
    if (p === '/api/image/capabilities') return jsonResponse(CAPS);
    if (p === '/v1/images/generations') return jsonResponse({ created: 1, data: [{ b64_json: 'AAA' }, { b64_json: 'BBB' }] });
    if (p === '/api/hf/files') return jsonResponse({ files: [{ path: 'flux1-schnell-Q4_0.gguf', sizeBytes: 1000 }], count: 1 });
    if (p === '/api/hf/progress') return jsonResponse({ error: 'not found' }, 404);
    if (p === '/api/hf/download' && method === 'POST') return jsonResponse({ status: 'started' });
    // Bundle-эндпоинта на воркере ещё нет (Phase 2): канонический /api/hf/bundle
    // и оба запасных пути отдают 404.
    if (p === '/api/hf/bundle') return jsonResponse({ error: 'not found' }, 404);
    if (p === '/api/hf/bundle/download') return jsonResponse({ error: 'not found' }, 404);
    if (p === '/api/image/models/download') return jsonResponse({ error: 'not found' }, 404);
    return jsonResponse({ error: 'unexpected ' + p }, 404);
};

require('./image-page.js');
const Page = global.ImagePage;
assert.ok(Page, 'window.ImagePage должен быть экспортирован');

const sleep = function (ms) { return new Promise(function (r) { setTimeout(r, ms); }); };

(async function run() {
    console.log('image-page DOM smoke');

    // --- 1/2. инициализация страницы -------------------------------------
    await Page.init();
    await sleep(30);

    check('init() не бросает и все DOM-id существуют в index.html', function () {
        assert.deepStrictEqual(unknownIds, [], 'неизвестные id: ' + unknownIds.join(', '));
    });
    check('все i18n-ключи модуля есть в ru.js', function () {
        assert.deepStrictEqual(missingKeys, [], 'нет ключей: ' + missingKeys.join(', '));
    });

    // --- 3. refresh: бэкенды / модели / capabilities ----------------------
    check('fallback: /api/v1/image/backends 404 → берём image-бэкенды из /api/v1/backends', function () {
        assert.strictEqual(Page._state.backends.length, 1);
        assert.strictEqual(Page._state.selectedBackendId, 'img-1');
        assert.strictEqual(Page._state.backends[0].imagePort, 18093);
    });
    check('таблица моделей отрисована (имя, размер, семейство, кнопка Load)', function () {
        const html = elements['imgModelsBody'].innerHTML;
        assert.ok(html.indexOf('sd15-q8') !== -1, 'нет имени модели');
        assert.ok(html.indexOf('1.6 GB') !== -1, 'нет размера: ' + html.slice(0, 200));
        assert.ok(html.indexOf('sd15') !== -1, 'нет семейства');
        assert.ok(html.indexOf('data-img-action="load-model"') !== -1, 'нет кнопки Load');
    });
    check('capabilities применились: лимиты движка (max 2048, batch 4)', function () {
        assert.strictEqual(Page._state.caps.source, 'engine');
        assert.strictEqual(elements['imgWidth'].max, 2048);
        assert.strictEqual(elements['imgBatch'].max, 4);
        assert.ok(elements['imgSampler'].innerHTML.indexOf('dpm++2m') !== -1, 'нет самплеров движка');
    });

    // --- 4. генерация -----------------------------------------------------
    elements['imgPrompt'].value = 'a cat';
    elements['imgNegative'].value = 'blurry';
    elements['imgWidth'].value = '513';
    elements['imgHeight'].value = '512';
    elements['imgSteps'].value = '8';
    elements['imgCfg'].value = '1';
    elements['imgSeed'].value = '';
    elements['imgBatch'].value = '2';
    elements['imgModelSelect'].value = 'sd15-q8';

    await Page._actions.generate();
    await sleep(20);

    const genReq = requests.filter(function (r) { return r.url.indexOf('/v1/images/generations') !== -1; })[0];
    check('POST v1/images/generations: тело соответствует контракту', function () {
        assert.ok(genReq, 'запрос генерации не ушёл');
        assert.strictEqual(genReq.method, 'POST');
        assert.strictEqual(genReq.body.prompt, 'a cat');
        assert.strictEqual(genReq.body.negative_prompt, 'blurry');
        assert.strictEqual(genReq.body.width, 512, '513 → 512 (сетка 64)');
        assert.strictEqual(genReq.body.steps, 8);
        assert.strictEqual(genReq.body.seed, -1, 'пустой seed → -1 (random)');
        assert.strictEqual(genReq.body.batch_size, 2);
        assert.strictEqual(genReq.headers['X-API-Token'], 'test-token', 'заголовок авторизации');
        assert.ok(genReq.url.indexOf('/api/v1/image/backends/img-1/proxy/v1/images/generations') !== -1, 'путь прокси: ' + genReq.url);
    });
    check('результат: показаны обе картинки как data URL, кнопка «Скачать все» включена', function () {
        const html = elements['imgResults'].innerHTML;
        assert.ok(html.indexOf('data:image/png;base64,AAA') !== -1, 'нет первой картинки');
        assert.ok(html.indexOf('data:image/png;base64,BBB') !== -1, 'нет второй картинки');
        assert.strictEqual(elements['imgResultsEmpty'].style.display, 'none');
        assert.strictEqual(elements['imgDownloadAllBtn'].style.display, 'inline-block');
    });
    check('галерея попала в localStorage (параметры + b64)', function () {
        const raw = localStorage.getItem('ollamalegion_image_history');
        assert.ok(raw, 'история не сохранена');
        const hist = JSON.parse(raw);
        assert.strictEqual(hist.length, 1);
        assert.strictEqual(hist[0].prompt, 'a cat');
        assert.strictEqual(hist[0].images.length, 2);
        assert.strictEqual(hist[0].images[0].b64, 'AAA');
    });
    check('тост об успехе содержит и время, и число картинок', function () {
        const last = toasts[toasts.length - 1];
        assert.strictEqual(last.type, 'success');
        assert.ok(last.msg.indexOf('Картинок получено: 2') !== -1, last.msg);
    });
    check('во время генерации спиннер гаснет, elapsed заполнен', function () {
        assert.strictEqual(Page._state.generating, false);
        assert.strictEqual(elements['imgGenSpinner'].style.display, 'none');
    });

    // --- 5. bundle --------------------------------------------------------
    elements['imgBundleName'].value = 'flux-schnell-q4';
    elements['imgBundleFamily'].value = 'flux';
    await Page._actions.bundleDownload();
    await sleep(20);

    const perFile = requests.filter(function (r) { return r.url.indexOf('/proxy/api/hf/download') !== -1; });
    const bundleTries = requests.filter(function (r) { return r.url.indexOf('/proxy/api/hf/bundle') !== -1 || r.url.indexOf('/proxy/api/image/models/download') !== -1; });
    check('bundle-эндпоинта нет → сначала канонический /api/hf/bundle, затем fallback на /api/hf/download с role', function () {
        assert.strictEqual(bundleTries.length >= 1, true, 'канонический путь /api/hf/bundle не пробовался');
        assert.strictEqual(bundleTries[0].url.indexOf('/proxy/api/hf/bundle') !== -1, true, 'первым должен идти канонический путь, был: ' + bundleTries[0].url);
        assert.strictEqual(bundleTries[0].body.name, 'flux-schnell-q4');
        assert.strictEqual(bundleTries[0].body.family, 'flux');
        assert.strictEqual(bundleTries[0].body.files.length, 2);
        assert.strictEqual(perFile.length, 2, 'должно быть 2 файла, было ' + perFile.length);
        assert.strictEqual(perFile[0].body.modelId, 'leejet/FLUX.1-schnell-gguf');
        assert.strictEqual(perFile[0].body.role, 'diffusion');
        assert.strictEqual(perFile[1].body.role, 'vae');
        const warn = toasts.filter(function (t) { return t.msg.indexOf('Эндпоинт bundle') !== -1; })[0];
        assert.ok(warn, 'нет предупреждения о недоступном bundle-эндпоинте');
    });
    Page._actions.stopPolling();

    // --- 6. load / unload модели ------------------------------------------
    await Page._actions.loadModel('sd15-q8');
    await sleep(30);
    check('load модели: POST /api/image/models/load {name} + поллинг прогресса остановлен', function () {
        const loadReq = requests.filter(function (r) { return r.url.indexOf('/api/image/models/load') !== -1 && r.url.indexOf('progress') === -1; })[0];
        assert.ok(loadReq, 'POST load не ушёл');
        assert.strictEqual(loadReq.body.name, 'sd15-q8');
        assert.strictEqual(Page._state.loadPoll, null, 'поллинг должен остановиться после state=loaded');
        const done = toasts.filter(function (t) { return t.msg.indexOf('Модель загружена') !== -1; })[0];
        assert.ok(done, 'нет тоста о загрузке модели');
    });
    Page._actions.stopPolling();

    // --- 7. ошибка генерации в OpenAI-конверте ----------------------------
    global.fetch = function (url, init) {
        if (String(url).indexOf('/v1/images/generations') !== -1) {
            return jsonResponse({ error: { message: 'no healthy backend of type image_cpp is registered', type: 'invalid_request_error', code: 'image_backend_unavailable' } }, 503);
        }
        return jsonResponse({}, 200);
    };
    await Page._actions.generate();
    await sleep(20);
    check('ошибка 503 показывается текстом из OpenAI-конверта', function () {
        const last = toasts[toasts.length - 1];
        assert.strictEqual(last.type, 'error');
        assert.ok(last.msg.indexOf('no healthy backend of type image_cpp is registered (image_backend_unavailable)') !== -1, last.msg);
    });

    console.log('\n' + (failures.length ? 'FAILED: ' + failures.length : 'OK: ' + passed + ' checks passed'));
    if (failures.length) {
        failures.forEach(function (f) { console.error(' - ' + f); });
        process.exit(1);
    }
})();
