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
    'imageModels.selftest_no_backend': 'Выберите image-бэкенд',
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

global.fetch = function (url, init) {
    const rec = {
        url: String(url),
        method: (init && init.method) || 'GET',
        headers: (init && init.headers) || {},
        body: init && init.body ? JSON.parse(init.body) : null,
    };
    requests.push(rec);
    return Promise.resolve({
        ok: nextResponse.status < 400,
        status: nextResponse.status,
        text: function () { return Promise.resolve(JSON.stringify(nextResponse.body)); },
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

    check('табы: их шесть и все панели найдены', function () {
        assert.deepStrictEqual(Page.tabIds, ['overview', 'hf', 'models', 'loaded', 'downloads', 'settings']);
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

    await checkAsync('runSelfTest: один запрос на generate с 64x64/1 шаг/sync и токеном', async function () {
        requests.length = 0;
        nextResponse = { status: 200, body: { status: 'ok', async: false, images: [{ b64_json: 'BASE64_СЕКРЕТ' }] } };
        getEl('imSelfTestResult').textContent = '';
        const ok = await Page.runSelfTest();
        assert.strictEqual(ok, true, 'проверка не прошла');
        assert.strictEqual(requests.length, 1, 'запросов: ' + requests.length);
        const req = requests[0];
        assert.ok(req.url.indexOf('/api/v1/image/backends/img1/generate') !== -1, 'неверный путь: ' + req.url);
        assert.strictEqual(req.method, 'POST');
        assert.strictEqual(req.body.sync, true, 'нет sync:true (иначе пришлось бы поллить джобу)');
        assert.strictEqual(req.body.width, 64);
        assert.strictEqual(req.body.height, 64);
        assert.strictEqual(req.body.steps, 1);
        assert.ok(req.headers['X-API-Token'], 'не передан токен');
        const txt = getEl('imSelfTestResult').textContent;
        assert.ok(txt.indexOf('OK') === 0, 'нет отметки OK в начале: ' + txt);
        assert.ok(/OK — \d+ мс/.test(txt), 'нет времени выполнения: ' + txt);
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

    await checkAsync('без выбранного бэкенда запрос не уходит', async function () {
        requests.length = 0;
        getEl('imgBackendSelect').value = '';
        Page.render([{ id: 'a', backendType: 'image_cpp' }, { id: 'b', backendType: 'image_cpp' }]);
        const ok = await Page.runSelfTest();
        assert.strictEqual(ok, false);
        assert.strictEqual(requests.length, 0, 'запрос ушёл без выбранного бэкенда');
        assert.strictEqual(getEl('imSelfTestResult').textContent, 'Выберите image-бэкенд');
    });

    console.log('');
    if (process.exitCode) {
        console.log('ИТОГ: есть провалы');
    } else {
        console.log('ИТОГ: все проверки пройдены (' + passed + ')');
    }
})();
