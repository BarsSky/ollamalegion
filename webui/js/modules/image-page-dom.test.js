// image-page-dom.test.js — R-Image Phase 9 (2026-10-03): интеграционный smoke
// страницы «Image-модели» с мок-DOM и мок-fetch (без браузера).
//
// Запуск: node webui/js/modules/image-page-dom.test.js
//
// Зачем этот тест, если есть image-page.test.js:
//   * чистые функции не ловят опечатки в id элементов и в вызовах DOM —
//     а именно они дают «страница молча не работает»;
//   * здесь проверяется весь путь: refresh() → рендер таблицы моделей →
//     load/unload и прогресс загрузки → монтирование редактора профилей.
//
// ГРАНИЦА С image-models-hf.test.js: загрузка bundle с HuggingFace (поиск репо,
// выбор файлов с ролями, прогресс, история/остатки) в Phase 9 переехала в
// отдельный таб-модуль image-models-hf.js — её путь проверяется ТАМ. Здесь
// остаётся ровно то, чем владеет image-page.js (табы «Модели на диске»,
// «Загруженные», «Настройки»).
//
// Что проверяем:
//   1. Каждый getElementById из модуля существует в webui/index.html — ловит
//      рассинхрон HTML/JS.
//   2. Каждый i18n-ключ, который дёргает модуль, есть в ru.js (поэтому тест
//      грузит реальный пак переводов, а не заглушку).
//   3. refresh(): список image-бэкендов (fallback на /api/v1/backends при 404),
//      модели, таблица моделей и селект модели.
//   4. Прогресс загрузки модели: стадия/время в подписи, полоса — только если
//      воркер реально прислал progressPct.
//   5. load/unload модели: POST-тела и остановка поллинга по state=loaded.
//   6. HF/bundle-логики в модуле больше НЕТ (она у image-models-hf.js) — это и
//      есть проверка «ничего не потеряли, а перенесли».
//   7. В модуле НЕ осталось генерации, результата и галереи (грепом по исходнику:
//      нет imgGenerateBtn, imgGallery, prompt_required, /generate и т.п.) — это
//      главное требование Phase 9: картинки в WebUI не показываем.
//   8. Сохранённый функционал на месте: модели, прогресс загрузки, профили.

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

// Редактор профилей (image-profiles.js) в этом тесте не грузим: проверяем ровно
// то, что init() зовёт его mount() с контейнером таба «Настройки».
global.ImageProfiles = {
    mounted: [],
    mount: function (id) { this.mounted.push(id); },
};

// --- mocks: EventSource (SSE-прогресс загрузки модели) ----------------------
// Модуль предпочитает SSE и откатывается на polling. Фейковый EventSource
// записывает URL и позволяет «прислать» сообщение руками — так проверяется и
// транспорт (URL со ?token=), и реакция на терминальное состояние, и откат.
global.EventSource = undefined;
const streams = [];
function FakeEventSource(url) {
    this.url = String(url);
    this.readyState = 0; // CONNECTING
    this.onopen = null;
    this.onmessage = null;
    this.onerror = null;
    this.closed = false;
    streams.push(this);
}
FakeEventSource.CONNECTING = 0;
FakeEventSource.OPEN = 1;
FakeEventSource.CLOSED = 2;
FakeEventSource.prototype.close = function () { this.closed = true; this.readyState = 2; };
FakeEventSource.prototype.emit = function (obj) {
    if (this.onmessage) this.onmessage({ data: JSON.stringify(obj) });
};
FakeEventSource.prototype.fail = function () {
    this.readyState = 2; // CLOSED: браузер переподключаться не станет
    if (this.onerror) this.onerror({});
};

// --- mocks: DOM ------------------------------------------------------------
const MODULE_PATH = path.join(__dirname, 'image-page.js');
const MODULE_SRC = fs.readFileSync(MODULE_PATH, 'utf8');
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

// Заранее создаём все элементы, которые есть в index.html, чтобы тест мог
// читать/записывать их value и innerHTML. Обращение модуля к id, которого нет
// в index.html, всё равно фиксируется в unknownIds (см. getElementById).
KNOWN_IDS.forEach(function (id) { elements[id] = makeElement(id); });

global.document = {
    getElementById: function (id) {
        if (!Object.prototype.hasOwnProperty.call(elements, id)) {
            if (!KNOWN_IDS.has(id)) unknownIds.push(id);
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
        // Состав bundle: роли приходят с сервера (types.ImageModelFile.Role).
        files: [{ role: 'diffusion', path: 'sd15-q8.gguf' }, { role: 'vae', path: 'vae.safetensors' }],
    }],
};

// confirm() для удаления bundle: тест управляет ответом оператора.
let confirmAnswer = true;
const confirmCalls = [];
global.confirm = function (msg) { confirmCalls.push(msg); return confirmAnswer; };

let imageBackendsStatus = 404; // /api/v1/image/backends ещё нет (параллельная работа)
// Прогресс загрузки модели: 'loading' — первый снимок со стадией и процентом,
// 'loaded' — финальный (по нему поллинг обязан остановиться).
let loadProgressMode = 'loading';

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
        if (loadProgressMode === 'loading') {
            return jsonResponse({
                progress: { state: 'loading', model: 'sd15-q8', stage: 'spawning sd-server', elapsed_ms: 4200, progressPct: 42, events: [] },
                state: 'loading',
                model: 'sd15-q8',
            });
        }
        return jsonResponse({
            progress: { state: 'loaded', model: 'sd15-q8', stage: 'ready', elapsed_ms: 5200, events: [] },
            state: 'loaded',
            model: 'sd15-q8',
            pid: 4242,
        });
    }
    if (p === '/api/image/models/load') return jsonResponse({ status: 'ok' });
    if (p === '/api/image/models/unload') return jsonResponse({ status: 'ok' });
    if (p === '/api/image/models/delete') return jsonResponse({ status: 'ok', name: 'sd15-q8', freed_bytes: 1760000000 });
    if (p === '/api/image/models') return jsonResponse(MODELS);
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

    // --- 3. refresh: бэкенды / модели / селекты ---------------------------
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
    check('состав bundle (роли) и кнопка удаления с диска видны для незагруженной модели', function () {
        const html = elements['imgModelsBody'].innerHTML;
        assert.ok(html.indexOf('diffusion (веса)') !== -1, 'нет роли diffusion в составе');
        assert.ok(html.indexOf('vae') !== -1, 'нет роли vae в составе');
        assert.ok(html.indexOf('data-img-action="delete-model"') !== -1, 'нет кнопки удаления с диска');
        assert.strictEqual((html.match(/colspan="8"/g) || []).length, 0, 'colspan не должен попадать в строки с данными');
    });
    check('селекты шапки заполнены: бэкенд и модель', function () {
        assert.ok(elements['imgBackendSelect'].innerHTML.indexOf('img-1') !== -1, 'нет бэкенда в селекте');
        assert.ok(elements['imgModelSelect'].innerHTML.indexOf('sd15-q8') !== -1, 'нет модели в селекте');
    });
    check('профили image-моделей смонтированы в таб «Настройки»', function () {
        assert.deepStrictEqual(global.ImageProfiles.mounted, ['imageProfiles']);
    });

    // --- 4. прогресс загрузки модели --------------------------------------
    loadProgressMode = 'loading';
    await Page._actions.loadModel('sd15-q8');
    await sleep(30);
    check('load модели: POST /api/image/models/load {name}', function () {
        const loadReq = requests.filter(function (r) { return r.url.indexOf('/proxy/api/image/models/load') !== -1 && r.url.indexOf('progress') === -1; })[0];
        assert.ok(loadReq, 'POST load не ушёл');
        assert.strictEqual(loadReq.method, 'POST');
        assert.strictEqual(loadReq.body.name, 'sd15-q8');
        assert.strictEqual(loadReq.headers['X-API-Token'], 'test-token', 'заголовок авторизации');
    });
    check('прогресс загрузки: стадия и время в подписи, полоса по progressPct воркера', function () {
        assert.strictEqual(elements['imgLoadProgress'].style.display, '', 'блок прогресса должен быть виден');
        assert.ok(elements['imgLoadProgressLabel'].textContent.indexOf('spawning sd-server') !== -1,
            'нет стадии в подписи: ' + elements['imgLoadProgressLabel'].textContent);
        assert.strictEqual(elements['imgLoadProgressBar'].style.display, '', 'полоса должна быть видна при progressPct');
        assert.strictEqual(elements['imgLoadProgressFill'].style.width, '42%');
        assert.ok(Page._state.loadPoll, 'поллинг прогресса должен идти, пока state=loading');
    });
    Page._actions.stopPolling();

    // --- 5. завершение загрузки: наблюдение останавливается ----------------
    loadProgressMode = 'loaded';
    await Page._actions.loadModel('sd15-q8');
    await sleep(30);
    check('state=loaded → наблюдение остановлено, тост о загрузке', function () {
        assert.strictEqual(Page._state.loadPoll, null, 'наблюдение должно остановиться после state=loaded');
        const done = toasts.filter(function (t) { return t.msg.indexOf('Модель загружена') !== -1; })[0];
        assert.ok(done, 'нет тоста о загрузке модели');
    });

    // --- 5b. SSE-прогресс: основной транспорт (Phase 9) --------------------
    // Модуль предпочитает SSE (/progress/stream) и откатывается на polling
    // только если EventSource недоступен или поток закрылся.
    global.EventSource = FakeEventSource;
    streams.length = 0;
    loadProgressMode = 'loading';
    await Page._actions.loadModel('sd15-q8');
    await sleep(20);
    check('SSE: подписка на /progress/stream через прокси балансера и с ?token=', function () {
        assert.strictEqual(streams.length, 1, 'EventSource не создан');
        assert.ok(streams[0].url.indexOf('/proxy/api/image/models/load/progress/stream') !== -1,
            'неверный URL потока: ' + streams[0].url);
        assert.ok(streams[0].url.indexOf('token=test-token') !== -1,
            'EventSource не умеет заголовки — токен обязан быть в query: ' + streams[0].url);
        assert.strictEqual(Page._state.loadPoll.mode, 'sse', 'режим должен быть SSE');
        assert.strictEqual(Page._state.loadPoll.timer, null, 'при SSE опрос не запускается');
    });
    check('SSE: сообщение прогресса рисуется без опроса', function () {
        const before = requests.length;
        streams[0].emit({ state: 'loading', model: 'sd15-q8', stage: 'loading weights', elapsed_ms: 7100 });
        assert.strictEqual(requests.length, before, 'SSE-обновление не должно порождать HTTP-запросы');
        assert.ok(elements['imgLoadProgressLabel'].textContent.indexOf('loading weights') !== -1,
            'стадия из SSE не отрисована: ' + elements['imgLoadProgressLabel'].textContent);
        assert.ok(elements['imgLoadProgressLabel'].textContent.indexOf('7s') !== -1,
            'время из SSE не отрисовано: ' + elements['imgLoadProgressLabel'].textContent);
    });
    check('SSE: без времени в подписи нет пустых скобок', function () {
        streams[0].emit({ state: 'loading', model: 'sd15-q8', stage: 'spawn', elapsed_ms: 0 });
        const txt = elements['imgLoadProgressLabel'].textContent;
        assert.ok(txt.indexOf('spawn') !== -1, 'стадия потеряна: ' + txt);
        assert.strictEqual(txt.indexOf('()'), -1, 'пустые скобки в подписи: ' + txt);
    });
    check('SSE: терминальное состояние закрывает поток и перечитывает модели', function () {
        streams[0].emit({ state: 'loaded', model: 'sd15-q8', stage: 'ready', elapsed_ms: 8200 });
        assert.strictEqual(Page._state.loadPoll, null, 'поток должен быть остановлен');
        assert.strictEqual(streams[0].closed, true, 'EventSource должен быть закрыт');
        assert.ok(toasts.some(function (t) { return t.msg.indexOf('Модель загружена') !== -1; }), 'нет тоста о загрузке');
    });
    // Асинхронная подготовка — отдельно, проверки остаются синхронными
    // (check() ловит только синхронные исключения).
    streams.length = 0;
    loadProgressMode = 'loading';
    await Page._actions.loadModel('sd15-q8');
    await sleep(20);
    const freshStream = streams[0];
    freshStream.fail(); // CLOSED: браузер переподключаться не станет
    check('SSE: закрытый поток (4xx/нет ручки) откатывается на polling', function () {
        assert.strictEqual(streams.length, 1, 'новая подписка не создана');
        assert.ok(Page._state.loadPoll, 'наблюдение за прогрессом потеряно');
        assert.strictEqual(Page._state.loadPoll.mode, 'poll', 'должен включиться polling-fallback');
        assert.ok(Page._state.loadPoll.timer, 'таймер опроса не запущен');
    });
    Page._actions.stopPolling();
    global.EventSource = undefined;

    // --- 6. unload модели -------------------------------------------------
    await Page._actions.unloadModel('sd15-q8');
    await sleep(30);
    check('unload модели: POST /api/image/models/unload {name} + перечитывание списка', function () {
        const unloadReq = requests.filter(function (r) { return r.url.indexOf('/proxy/api/image/models/unload') !== -1; })[0];
        assert.ok(unloadReq, 'POST unload не ушёл');
        assert.strictEqual(unloadReq.body.name, 'sd15-q8');
        const unloaded = toasts.filter(function (t) { return t.msg.indexOf('Модель выгружена') !== -1; })[0];
        assert.ok(unloaded, 'нет тоста о выгрузке модели');
    });

    // --- 7. удаление bundle с диска (Phase 9) ------------------------------
    // Сначала отказ оператора: запроса быть не должно.
    confirmAnswer = false;
    requests.length = 0;
    await Page._actions.deleteModel('sd15-q8');
    check('удаление: отказ в confirm() → запроса нет', function () {
        assert.strictEqual(requests.filter(function (r) { return r.url.indexOf('/models/delete') !== -1; }).length, 0);
        assert.ok(confirmCalls.length >= 1, 'оператора не спросили');
    });
    check('удаление: в тексте подтверждения есть имя bundle', function () {
        assert.ok(confirmCalls[confirmCalls.length - 1].indexOf('sd15-q8') !== -1,
            'нет имени в подтверждении: ' + confirmCalls[confirmCalls.length - 1]);
    });

    confirmAnswer = true;
    requests.length = 0;
    const deleted = await Page._actions.deleteModel('sd15-q8');
    await sleep(20);
    check('удаление: POST /models/delete {name} + тост с освобождённым объёмом', function () {
        assert.strictEqual(deleted, true);
        const rec = requests.filter(function (r) { return r.url.indexOf('/proxy/api/image/models/delete') !== -1; })[0];
        assert.ok(rec, 'POST /api/image/models/delete не ушёл');
        assert.strictEqual(rec.method, 'POST');
        assert.strictEqual(rec.body.name, 'sd15-q8');
        const done = toasts.filter(function (t) { return t.msg.indexOf('Удалено, освобождено') !== -1; })[0];
        assert.ok(done, 'нет тоста об освобождённом объёме');
        assert.ok(done.msg.indexOf('1678') !== -1, 'объём должен быть в MB: ' + done.msg);
    });
    check('удаление: список моделей перечитан (модель не остаётся фантомом)', function () {
        assert.ok(requests.filter(function (r) { return r.url.indexOf('/proxy/api/image/models') !== -1 && r.url.indexOf('delete') === -1; }).length >= 1,
            'список моделей не перечитан после удаления');
    });

    // --- 8. таб «Настройки»: сводка параметров бэкенда ---------------------
    check('«Настройки»: сводка параметров выбранного бэкенда и вход в его редактор', function () {
        Page._actions.renderBackendParamsState();
        const txt = elements['imBackendParamsState'].textContent;
        assert.ok(txt.indexOf('GPU image') !== -1, 'нет имени бэкенда: ' + txt);
        assert.ok(txt.indexOf('18093') !== -1, 'нет порта воркера: ' + txt);
        assert.ok(KNOWN_IDS.has('imBackendParamsBtn'), 'в index.html нет кнопки параметров бэкенда');
        assert.ok(MODULE_SRC.indexOf('openEditor') !== -1, 'модуль не открывает редактор бэкенда');
    });

    // --- 9. HF/bundle перенесён в image-models-hf.js -----------------------
    // Владелец сменился: ручная форма «строка = repo+filename+роль» больше не
    // живёт в image-page.js. Проверяем и исходник, и разметку — так поймаем и
    // «половину переноса» (код убрали, а поля остались), и «двойного владельца»
    // (оба модуля слушают #imgBundleStart).
    check('HF/bundle-логики в image-page.js больше нет', function () {
        // Комментарии не считаем: в них Phase 9 как раз объясняет, КУДА уехал
        // HF-код. Ищем только в исполняемых строках.
        const code = MODULE_SRC.split('\n').filter(function (line) {
            const s = line.trim();
            return s.indexOf('//') !== 0 && s.indexOf('*') !== 0 && s.indexOf('/*') !== 0;
        }).join('\n');
        ['onBundleDownload', 'listBundleFiles', 'renderBundleRows', 'readBundleRows',
            'parseBundleRows', 'aggregateDownloadProgress', "/api/hf/", 'imgBundleRows',
            'imgBundleAddRow'].forEach(function (tok) {
            assert.strictEqual(code.indexOf(tok), -1, 'в image-page.js остался ' + tok);
        });
    });
    check('разметка HF-таба на месте и принадлежит модулю image-models-hf.js', function () {
        ['imHfQuery', 'imHfTask', 'imHfSearchBtn', 'imHfSearchResults',
            'imHfFilesCard', 'imHfRepoName', 'imHfFilesBody', 'imHfSelectionSummary',
            'imgBundleName', 'imgBundleFamily', 'imgHfToken', 'imgBundleStart',
            'imgBundleProgress', 'imgBundleProgressFill', 'imgBundleStatus',
            'imDownloadsHost'].forEach(function (id) {
            assert.ok(KNOWN_IDS.has(id), 'в index.html нет #' + id);
        });
        const hfSrc = fs.readFileSync(path.join(__dirname, 'image-models-hf.js'), 'utf8');
        assert.ok(hfSrc.indexOf('data-imh-action="download"') === -1 || hfSrc.indexOf("act === 'download'") !== -1,
            'модуль HF не обрабатывает кнопку скачивания bundle');
        assert.ok(hfSrc.indexOf('/hf/bundle') !== -1, 'модуль HF не зовёт /hf/bundle');
    });

    // --- 9. ошибка бэкенда показывается в notice --------------------------
    global.fetch = function (url, init) {
        const u = String(url);
        // Порядок важен: прокси-URL моделей содержит и '/api/v1/image/backends'.
        if (u.indexOf('/api/image/models') !== -1) return jsonResponse({ error: 'worker is down' }, 500);
        if (u.indexOf('/api/v1/image/backends') !== -1) {
            return jsonResponse({ backends: [{ id: 'img-1', name: 'GPU image', type: 'image_cpp', host: '10.0.0.2', imagePort: 18093 }] });
        }
        return jsonResponse({}, 200);
    };
    await Page._actions.refresh();
    await sleep(30);
    check('недоступный воркер: ошибка видна в notice, страница не падает', function () {
        assert.strictEqual(elements['imgNotice'].style.display, '', 'notice должен быть показан');
        assert.ok(elements['imgNotice'].innerHTML.indexOf('worker is down') !== -1,
            'нет текста ошибки: ' + elements['imgNotice'].innerHTML);
    });

    // --- 10. генерации/галереи в модуле больше нет ------------------------
    check('в модуле не осталось генерации, результата и галереи', function () {
        const forbidden = [
            'imgGenerateBtn', 'imgGenSpinner', 'imgGenElapsed',
            'imgResults', 'imgResultsEmpty', 'imgDownloadAllBtn',
            'imgGallery', 'imgGalleryEmpty', 'imgGalleryClear',
            'imgPrompt', 'imgNegative', 'imgWidth', 'imgHeight', 'imgSteps',
            'imgCfg', 'imgSeed', 'imgBatch', 'imgSampler', 'imgScheduler', 'imgCapsNote',
            'prompt_required', '/generate', 'reuse_params',
            'ollamalegion_image_history', 'ollamalegion_image_form',
            'download_one', 'gallery_no_preview',
        ];
        const found = forbidden.filter(function (tok) { return MODULE_SRC.indexOf(tok) !== -1; });
        assert.deepStrictEqual(found, [], 'в image-page.js остались следы генерации/галереи: ' + found.join(', '));
    });

    // --- 8. сохранённый функционал на месте -------------------------------
    check('публичный API сохранён (init/refresh/pure/_actions), generate удалён', function () {
        assert.strictEqual(typeof Page.init, 'function', 'нет init');
        assert.strictEqual(typeof Page.refresh, 'function', 'нет refresh');
        assert.strictEqual(typeof Page.pure, 'object', 'нет pure');
        assert.strictEqual(typeof Page._actions.refresh, 'function', 'нет _actions.refresh');
        assert.strictEqual(typeof Page._actions.loadModel, 'function', 'нет _actions.loadModel');
        assert.strictEqual(typeof Page._actions.unloadModel, 'function', 'нет _actions.unloadModel');
        assert.strictEqual(typeof Page._actions.stopPolling, 'function', 'нет _actions.stopPolling');
        assert.strictEqual(Page._actions.generate, undefined, 'генерация должна быть удалена');
        assert.strictEqual(Page._actions.bundleDownload, undefined, 'bundle-загрузка должна была переехать в image-models-hf.js');
    });
    check('pure-хелперы сохранённого функционала на месте', function () {
        ['normalizeModels', 'normalizeBackends', 'stateLabelKey', 'normalizeLoadProgress',
            'formatBytes', 'formatDuration', 'parseOpenAIError', 'escapeHtml']
            .forEach(function (name) {
                assert.notStrictEqual(Page.pure[name], undefined, 'pure.' + name + ' потерян');
            });
        // Роли и семейства — контракт pkg/types/image_model.go. После переноса HF
        // они живут в image-models-hf.js (там же проверяются на 12 значений).
        assert.strictEqual(Page.pure.IMAGE_ROLES, undefined, 'роли должны были переехать в image-models-hf.js');
    });
    check('модуль по-прежнему владеет id моделей, прогресса и профилей', function () {
        assert.ok(KNOWN_IDS.has('imgModelsTable'), 'в index.html нет таблицы #imgModelsTable');
        assert.ok(KNOWN_IDS.has('imTabModels'), 'в index.html нет таба «Модели на диске»');
        assert.ok(KNOWN_IDS.has('imTabLoaded'), 'в index.html нет таба «Загруженные»');
        assert.ok(KNOWN_IDS.has('imTabSettings'), 'в index.html нет таба «Настройки»');
        ['imgModelsBody', 'imgModelsRefreshBtn',
            'imgLoadProgress', 'imgLoadProgressLabel', 'imgLoadProgressBar',
            'imgLoadProgressFill', 'imgNotice', 'imgBackendSelect', 'imgModelSelect']
            .forEach(function (tok) {
                assert.ok(MODULE_SRC.indexOf(tok) !== -1, 'в модуле нет ' + tok);
            });
        assert.ok(MODULE_SRC.indexOf("ImageProfiles.mount('imageProfiles')") !== -1, 'нет монтирования профилей');
    });

    console.log('\n' + (failures.length ? 'FAILED: ' + failures.length : 'OK: ' + passed + ' checks passed'));
    if (failures.length) {
        failures.forEach(function (f) { console.error(' - ' + f); });
        process.exit(1);
    }
})();
