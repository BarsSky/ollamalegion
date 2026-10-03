// image-models-page.test.js — R-Image Phase 9: шелл страницы «Image-модели».
//
// Запуск: node webui/js/modules/image-models-page.test.js
//
// ЧТО ПРОВЕРЯЕМ (то, что нельзя проверить в браузере автоматически):
//   1. Табы: клик по кнопке переключает класс .active у кнопки И у панели, выбор
//      запоминается в localStorage (в стиле страницы «GGUF модели»).
//   2. «Проверка бэкенда»: уходит РОВНО один запрос на нативный путь через
//      балансер (/api/v1/image/backends/{id}/generate) с телом 64x64/1 шаг/sync и
//      токеном; в UI показываются только OK и время.
//   3. ГЛАВНОЕ: сгенерированное изображение в WebUI НЕ рендерится — ответ с
//      base64 не попадает ни в один элемент страницы (Phase 9 убрала показ
//      картинок из WebUI: это задача клиентов).
//   4. Ошибка бэкенда показывается текстом, а не «тишиной».
'use strict';

const assert = require('assert');

// --- mocks: DOM -------------------------------------------------------------

function makeElement(id) {
    const classes = new Set();
    const listeners = {};
    const attrs = {};
    const el = {
        id: id,
        style: {},
        value: '',
        options: [],
        _html: '',
        textContent: '',
        getAttribute: function (n) { return Object.prototype.hasOwnProperty.call(attrs, n) ? attrs[n] : null; },
        setAttribute: function (n, v) { attrs[n] = v; },
        addEventListener: function (type, fn) { (listeners[type] = listeners[type] || []).push(fn); },
        removeEventListener: function () { },
        dispatch: function (type, ev) { (listeners[type] || []).forEach(function (fn) { fn(ev || {}); }); },
        classList: {
            add: function (c) { classes.add(c); },
            remove: function (c) { classes.delete(c); },
            contains: function (c) { return classes.has(c); },
            toggle: function (c, on) { if (on === undefined) { classes.has(c) ? classes.delete(c) : classes.add(c); } else if (on) { classes.add(c); } else { classes.delete(c); } },
        },
        appendChild: function () { },
        parentNode: null,
    };
    Object.defineProperty(el, 'innerHTML', {
        get: function () { return this._html; },
        set: function (v) { this._html = String(v); },
    });
    return el;
}

const elements = {};
function getEl(id) {
    if (!Object.prototype.hasOwnProperty.call(elements, id)) elements[id] = makeElement(id);
    return elements[id];
}
const selectorEls = {};
global.window = global;
global.document = {
    getElementById: getEl,
    querySelector: function (sel) {
        if (!Object.prototype.hasOwnProperty.call(selectorEls, sel)) selectorEls[sel] = makeElement(sel);
        return selectorEls[sel];
    },
    querySelectorAll: function () { return []; },
    createElement: function (tag) { return makeElement('created-' + tag); },
    addEventListener: function () { },
};
global.localStorage = {
    _d: {},
    getItem: function (k) { return Object.prototype.hasOwnProperty.call(this._d, k) ? this._d[k] : null; },
    setItem: function (k, v) { this._d[k] = String(v); },
    removeItem: function (k) { delete this._d[k]; },
};
global.WEBUI_CONFIG = { API_BASE: 'http://balancer.test:18081', API_TOKEN: 'test-token' };
// Словарь как в реальном ru.js (иначе модуль честно падает на свой fallback,
// и проверка текста перестала бы ловить подмену перевода).
const RU = {
    'imageModels.selftest_ok': 'OK',
    'imageModels.selftest_fail': 'Ошибка',
    'imageModels.selftest_running': 'Проверяю...',
    'imageModels.selftest_no_backend': 'Нет image-бэкендов',
    'imageModels.no_backend_selected': 'Image-бэкенд не выбран',
    'imageModels.ms': 'мс',
    'imageBackends.col_state': 'Состояние',
    'imageBackends.col_model': 'Текущая модель',
};
global.I18N = {
    t: function (k) { return Object.prototype.hasOwnProperty.call(RU, k) ? RU[k] : k; },
    getLang: function () { return 'ru'; },
};
global.console = console;

// --- mocks: fetch (с записью запросов) --------------------------------------

const requests = [];
let nextResponse = { status: 200, body: { status: 'ok', async: false, images: [{ b64_json: 'BASE64_СЕКРЕТ' }] } };
// Сценарные ответы: если задано, вызывается вместо nextResponse (нужно для
// последовательности «список моделей → load → список моделей → generate»).
let responseFor = null;

global.fetch = function (url, init) {
    const rec = {
        url: String(url),
        method: (init && init.method) || 'GET',
        headers: (init && init.headers) || {},
        body: init && init.body ? JSON.parse(init.body) : null,
    };
    requests.push(rec);
    const r = responseFor ? responseFor(rec) : nextResponse;
    return Promise.resolve({
        ok: r.status < 400,
        status: r.status,
        text: function () { return Promise.resolve(JSON.stringify(r.body)); },
    });
};

require('./image-models-page.js');
const Page = global.ImageModelsPage;
assert.ok(Page, 'window.ImageModelsPage должен быть экспортирован (см. check_iife_exports.py)');

// --- helpers ----------------------------------------------------------------

let passed = 0;
function check(name, fn) {
    try {
        fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        console.log('  \u2717 ' + name + ' — ' + (e && e.message));
        process.exitCode = 1;
    }
}

async function checkAsync(name, fn) {
    try {
        await fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        console.log('  \u2717 ' + name + ' — ' + (e && e.message));
        process.exitCode = 1;
    }
}

function tabBtn(tab) { return selectorEls['[data-im-tab="' + tab + '"]']; }
function tabPanel(tab) {
    return getEl('imTab' + tab.charAt(0).toUpperCase() + tab.slice(1));
}

(async function main() {
    console.log('image-models-page (шелл табов и самопроверки)');

    Page.mount();

    check('табы: их семь (включая «Тест») и все панели найдены', function () {
        assert.deepStrictEqual(Page.tabIds, ['overview', 'hf', 'models', 'loaded', 'downloads', 'settings', 'test']);
        Page.tabIds.forEach(function (tab) {
            assert.ok(tabBtn(tab), 'нет кнопки таба ' + tab);
            assert.ok(tabPanel(tab), 'нет панели таба ' + tab);
        });
    });

    check('showTab: активный класс переезжает на кнопку и панель', function () {
        Page.showTab('models');
        assert.ok(tabBtn('models').classList.contains('active'), 'кнопка models не активна');
        assert.ok(tabPanel('models').classList.contains('active'), 'панель models не активна');
        assert.strictEqual(tabPanel('overview').classList.contains('active'), false, 'панель overview осталась активной');
        assert.strictEqual(localStorage.getItem('ollamalegion_image_models_tab'), 'models', 'таб не запомнен');
    });

    check('showTab: неизвестный таб не ломает страницу (падает на overview)', function () {
        Page.showTab('нет-такого');
        assert.ok(tabPanel('overview').classList.contains('active'), 'не вернулись на overview');
    });

    // 2026-10-03: таб «Тест» — единственное место, где показывается картинка;
    // шелл обязан отдать ему контекст, иначе форма/результат останутся пустыми.
    check('таб «Тест»: шелл зовёт рендер модуля теста с контекстом таба', function () {
        const calls = [];
        window.ImageTestPage = {
            tabIds: ['test'],
            render: function (ctx) { calls.push({ tab: ctx && ctx.tab, backends: (ctx && ctx.backends || []).length, backendId: ctx && ctx.backendId }); },
        };
        try {
            getEl('imgBackendSelect').value = 'img1';
            Page.render([{ id: 'img1', backendType: 'image_cpp' }]); // активен overview → тест не зовём
            assert.strictEqual(calls.length, 0, 'тест рендерился на чужом табе: ' + JSON.stringify(calls));
            Page.showTab('test');
            assert.strictEqual(calls.length, 1, 'шелл не отдал контекст табу «Тест»');
            assert.strictEqual(calls[0].tab, 'test');
            assert.strictEqual(calls[0].backends, 1);
            assert.strictEqual(calls[0].backendId, 'img1');
        } finally {
            delete window.ImageTestPage;
            Page.showTab('overview');
        }
    });

    check('клик по кнопке таба переключает (делегирование на #imTabs)', function () {
        const tabs = getEl('imTabs');
        const target = makeElement('btn');
        target.setAttribute('data-im-tab', 'hf');
        target.parentNode = tabs;
        tabs.dispatch('click', { target: target });
        assert.ok(tabPanel('hf').classList.contains('active'), 'клик не переключил на hf');
    });

    check('render: состояние загруженной модели берётся из бэкендов кластера', function () {
        Page.render([
            { id: 'img1', backendType: 'image_cpp', image: { state: 'loaded', currentModel: 'sd15-q4', vramFreeMb: 894, vramTotalMb: 8192 } },
            { id: 'llm1', backendType: 'llama_cpp' },
        ]);
        getEl('imgBackendSelect').value = 'img1';
        Page.render([{ id: 'img1', backendType: 'image_cpp', image: { state: 'loaded', currentModel: 'sd15-q4', vramFreeMb: 894, vramTotalMb: 8192 } }]);
        const txt = getEl('imLoadedState').textContent;
        assert.ok(txt.indexOf('loaded') !== -1, 'нет состояния: ' + txt);
        assert.ok(txt.indexOf('sd15-q4') !== -1, 'нет текущей модели: ' + txt);
        assert.ok(txt.indexOf('894') !== -1 && txt.indexOf('8192') !== -1, 'нет VRAM: ' + txt);
    });

    // 2026-10-03: таб «Обзор» показывает «Подключение клиентов» для выбранного
    // image-бэкенда — тот же блок, что на странице «Бэкенды».
    check('таб «Обзор»: блок «Подключение клиентов» рисуется для выбранного бэкенда', function () {
        const calls = [];
        window.ClientAccess = {
            mount: function () { calls.push('mount'); },
            render: function (b) { calls.push(b && b.id); return '<div data-test-access="' + (b && b.id) + '"></div>'; },
        };
        try {
            getEl('imgBackendSelect').value = 'img1';
            Page.render([{ id: 'img1', backendType: 'image_cpp', host: 'legion' }]);
            assert.deepStrictEqual(calls.filter(function (c) { return c !== 'mount'; }), ['img1'],
                'ClientAccess.render вызван не для выбранного бэкенда: ' + JSON.stringify(calls));
            assert.ok(getEl('imClientAccessHost').innerHTML.indexOf('data-test-access="img1"') !== -1,
                'блок не отрисован в #imClientAccessHost: ' + getEl('imClientAccessHost').innerHTML);
        } finally {
            delete window.ClientAccess;
        }
    });

    await checkAsync('runSelfTest: при загруженной модели сразу генерация, без лишней загрузки', async function () {
        requests.length = 0;
        responseFor = null;
        nextResponse = { status: 200, body: { created: 1, data: [{ b64_json: 'BASE64_СЕКРЕТ', revised_prompt: '' }] } };
        getEl('imSelfTestResult').textContent = '';
        Page.render([{ id: 'img1', backendType: 'image_cpp', image: { state: 'loaded', currentModel: 'sd15-q4' } }]);
        const ok = await Page.runSelfTest();
        assert.strictEqual(ok, true, 'проверка не прошла');
        assert.strictEqual(requests.length, 1, 'запросов: ' + requests.length);
        const req = requests[0];
        // Клиентский путь: он считается метриками и проходит VRAM-гейт, поэтому
        // проверка оператора видна в Monitor. Управляющий алиас
        // /api/v1/image/backends/{id}/generate для этого не годится.
        assert.ok(/\/v1\/images\/generations$/.test(req.url), 'неверный путь: ' + req.url);
        assert.strictEqual(req.url.indexOf('/api/v1/image/backends/'), -1, 'проверка ушла в управляющий алиас: ' + req.url);
        assert.strictEqual(req.method, 'POST');
        assert.strictEqual(req.body.size, '64x64');
        assert.strictEqual(req.body.n, 1);
        assert.strictEqual(req.body.steps, 1, 'должен быть ровно 1 шаг');
        assert.ok(req.headers['X-API-Token'], 'не передан токен');
        const txt = getEl('imSelfTestResult').textContent;
        assert.ok(txt.indexOf('OK') === 0, 'нет отметки OK в начале: ' + txt);
        assert.ok(/OK — \d+ мс/.test(txt), 'нет времени выполнения: ' + txt);
        assert.ok(txt.indexOf('sd15-q4') !== -1, 'нет модели в результате: ' + txt);
    });

    await checkAsync('runSelfTest: без загруженной модели сначала load (гейт не пустит), затем генерация', async function () {
        requests.length = 0;
        responseFor = function (rec) {
            if (/\/models\/load$/.test(rec.url)) return { status: 202, body: { status: 'started', model: 'sd15-q4' } };
            if (/\/models$/.test(rec.url)) {
                // Первый опрос — модель ещё не загружена, второй — уже загружена.
                const loads = requests.filter(function (r) { return /\/models\/load$/.test(r.url); }).length;
                return loads === 0
                    ? { status: 200, body: { models: [{ name: 'sd15-q4' }], state: 'not_loaded', current_model: '' } }
                    : { status: 200, body: { models: [{ name: 'sd15-q4' }], state: 'loaded', current_model: 'sd15-q4' } };
            }
            return { status: 200, body: { created: 1, data: [{ b64_json: 'BASE64_СЕКРЕТ' }] } };
        };
        getEl('imSelfTestResult').textContent = '';
        Page.render([{ id: 'img1', backendType: 'image_cpp', image: { state: 'not_loaded', currentModel: '' } }]);
        const ok = await Page.runSelfTest();
        responseFor = null;
        assert.strictEqual(ok, true, 'проверка не прошла: ' + getEl('imSelfTestResult').textContent);
        const order = requests.map(function (r) { return r.method + ' ' + r.url.replace(/^.*\/api\/v1\/image\/backends\/img1/, '').replace(/^.*(\/v1\/images\/generations)$/, '$1'); });
        assert.strictEqual(order[0], 'GET /models', 'первым должен быть список моделей: ' + order.join(' | '));
        assert.strictEqual(order[1], 'POST /models/load', 'вторым — загрузка модели: ' + order.join(' | '));
        assert.ok(order[order.length - 1].indexOf('/v1/images/generations') !== -1, 'последним — генерация: ' + order.join(' | '));
        const txt = getEl('imSelfTestResult').textContent;
        assert.ok(txt.indexOf('OK') === 0, 'нет отметки OK: ' + txt);
        assert.ok(txt.indexOf('sd15-q4') !== -1, 'нет имени модели в результате: ' + txt);
    });

    await checkAsync('runSelfTest: транзиентный отказ гейта (кэш балансера) → повтор и успех', async function () {
        requests.length = 0;
        let gen = 0;
        responseFor = function (rec) {
            if (/\/v1\/images\/generations$/.test(rec.url)) {
                gen++;
                // Первый заход: кэш балансера ещё не знает о загруженной модели.
                if (gen === 1) {
                    return {
                        status: 503,
                        body: {
                            error: 'image_model_not_loaded',
                            message: 'no image model is loaded on the image backend (known models: sd15-q4)',
                            hint: 'load a model first: POST /api/image/models/load',
                            retry_after_seconds: 1,
                        },
                    };
                }
                return { status: 200, body: { created: 1, data: [{ b64_json: 'BASE64_СЕКРЕТ' }] } };
            }
            return { status: 200, body: { models: [], state: 'loaded', current_model: 'sd15-q4' } };
        };
        getEl('imSelfTestResult').textContent = '';
        Page.render([{ id: 'img1', backendType: 'image_cpp', image: { state: 'loaded', currentModel: 'sd15-q4' } }]);
        const ok = await Page.runSelfTest();
        const gens = requests.filter(function (r) { return /\/v1\/images\/generations$/.test(r.url); });
        responseFor = null;
        assert.strictEqual(ok, true, 'повтор не спас проверку: ' + getEl('imSelfTestResult').textContent);
        assert.strictEqual(gens.length, 2, 'должно быть 2 запроса генерации (отказ + повтор): ' + gens.length);
        assert.ok(getEl('imSelfTestResult').textContent.indexOf('OK') === 0, 'нет OK после повтора');
    });

    check('ПОКАЗ КАРТИНОК: base64 из ответа не попадает в DOM', function () {
        const seen = [];
        Object.keys(elements).forEach(function (id) { seen.push(elements[id].innerHTML + elements[id].textContent); });
        const dump = seen.join('|');
        assert.strictEqual(dump.indexOf('BASE64_СЕКРЕТ'), -1, 'изображение отрисовано в WebUI (Phase 9 это запрещает)');
        // Страховка от «ползучей» витрины: в модуле нет ни <img>, ни imgResults.
        const src = require('fs').readFileSync(require('path').join(__dirname, 'image-models-page.js'), 'utf8');
        assert.strictEqual(/<img/i.test(src), false, 'шелл начал рендерить <img>');
        assert.strictEqual(src.indexOf('imgResults'), -1, 'шелл ссылается на удалённый блок результатов');
    });

    await checkAsync('ошибка бэкенда показывается текстом', async function () {
        requests.length = 0;
        nextResponse = { status: 500, body: { error: 'engine_oom' } };
        getEl('imSelfTestResult').textContent = '';
        const ok = await Page.runSelfTest();
        assert.strictEqual(ok, false, 'ошибка не распознана');
        const txt = getEl('imSelfTestResult').textContent;
        assert.ok(txt.indexOf('Ошибка') === 0, 'нет отметки ошибки: ' + txt);
        assert.ok(txt.indexOf('engine_oom') !== -1, 'текст ошибки движка потерян: ' + txt);
    });

    await checkAsync('без image-бэкендов в кластере запрос не уходит', async function () {
        requests.length = 0;
        getEl('imgBackendSelect').value = '';
        Page.render([]);
        const ok = await Page.runSelfTest();
        assert.strictEqual(ok, false);
        assert.strictEqual(requests.length, 0, 'запрос ушёл без image-бэкендов');
        assert.strictEqual(getEl('imSelfTestResult').textContent, 'Нет image-бэкендов');
    });

    console.log('');
    if (process.exitCode) {
        console.log('ИТОГ: есть провалы');
    } else {
        console.log('ИТОГ: все проверки пройдены (' + passed + ')');
    }
})();
