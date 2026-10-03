// image-test-page.test.js — страница-инструмент «Image-тест».
//
// Run: node webui/js/modules/image-test-page.test.js
//
// ЗАЧЕМ (запрос оператора): «сделай тестовый запросчик к генератору картинок
// отдельной вкладкой, чтобы пользователь всегда мог проверить и протестировать
// работу настроек для модели и получить результат; включать её только если
// зарегистрирован соответствующий бэкенд». Проверяем:
//   1. видимость страницы/пункта меню зависит от наличия image_cpp-бэкенда;
//   2. тело запроса собирается правильно под каждую поверхность (OpenAI: size/n;
//      A1111: width/height/batch_size; нативный: sync) — «какие настройки уйдут»;
//   3. настройки профиля модели подставляются в форму («Из профиля»);
//   4. запрос уходит КЛИЕНТСКИМ путём (/v1/images/generations), а не в управляющий
//      алиас, и результат рисуется как <img>, с метаданными (время/seed/model);
//   5. ошибки движка показываются текстом + человеческой подсказкой (кейс
//      «get sd version from file failed» — GGUF для ComfyUI);
//   6. история теста живёт в памяти вкладки, «В форму» возвращает параметры.

'use strict';

const assert = require('assert');

// --- mocks: DOM / i18n / fetch ----------------------------------------------

function makeEl(id) {
    const attrs = {};
    const listeners = {};
    const el = {
        id: id,
        style: {},
        value: '',
        textContent: '',
        _html: '',
        disabled: false,
        getAttribute: function (n) { return Object.prototype.hasOwnProperty.call(attrs, n) ? attrs[n] : null; },
        setAttribute: function (n, v) { attrs[n] = v; },
        addEventListener: function (t, fn) { (listeners[t] = listeners[t] || []).push(fn); },
        removeEventListener: function () {},
        dispatch: function (t, ev) { (listeners[t] || []).forEach(function (fn) { fn(ev || {}); }); },
        classList: { add: function () {}, remove: function () {}, contains: function () { return false; }, toggle: function () {} },
        appendChild: function () {},
        parentNode: null,
    };
    Object.defineProperty(el, 'innerHTML', {
        get: function () { return this._html; },
        set: function (v) { this._html = String(v); },
    });
    return el;
}

const elements = {};
const navEls = {};
function getEl(id) {
    if (!Object.prototype.hasOwnProperty.call(elements, id)) elements[id] = makeEl(id);
    return elements[id];
}
global.window = global;
global.document = {
    getElementById: getEl,
    querySelector: function (sel) {
        if (!Object.prototype.hasOwnProperty.call(navEls, sel)) navEls[sel] = makeEl(sel);
        return navEls[sel];
    },
    querySelectorAll: function () { return []; },
    createElement: function (tag) { return makeEl('created-' + tag); },
    addEventListener: function () {},
    readyState: 'complete',
};
global.localStorage = { getItem: function () { return null; }, setItem: function () {}, removeItem: function () {} };
global.WEBUI_CONFIG = { API_BASE: '', API_TOKEN: 'test-key-1' };
global.location = { hostname: 'legion.host', origin: 'http://legion.host:18083' };
// В Node 22 `global.navigator` — getter-only, подменяем только clipboard.
try {
    Object.defineProperty(global.navigator, 'clipboard', { value: { writeText: function () { return Promise.resolve(); } }, configurable: true, writable: true });
} catch (e) {
    Object.defineProperty(global, 'navigator', { value: { clipboard: { writeText: function () { return Promise.resolve(); } } }, configurable: true, writable: true });
}

const RU = {
    'imageTest.no_models': 'нет моделей на диске',
    'imageTest.prompt_required': 'Укажите промпт',
    'imageTest.running': 'Генерация идёт…',
    'imageTest.done': 'Готово за {ms} мс',
    'imageTest.failed': 'Генерация не удалась',
    'imageTest.no_result': 'Результата ещё нет — нажмите «Сгенерировать».',
    'imageTest.defaults_applied': 'Подставлены настройки профиля модели',
    'imageTest.limits_note': 'Лимиты: {min}..{max} шаг {step}',
    'imageTest.caps_engine': 'возможности воркера',
    'imageTest.caps_static': 'значения по умолчанию',
    'imageModels.ms': 'мс',
    'image.load_started': 'Загрузка модели {name}…',
    'image.load_failed': 'Не удалось загрузить модель: {error}',
    'image.unload_failed': 'Не удалось выгрузить модель: {error}',
    'gguf.model_unloaded': 'Модель выгружена',
};
global.I18N = {
    t: function (k, vars) {
        var s = Object.prototype.hasOwnProperty.call(RU, k) ? RU[k] : k;
        if (vars) Object.keys(vars).forEach(function (p) { s = String(s).replace('{' + p + '}', vars[p]); });
        return s;
    },
};

const requests = [];
let responseFor = function () { return { status: 200, body: {} }; };
global.fetch = function (url, init) {
    const rec = { url: String(url), method: (init && init.method) || 'GET', headers: (init && init.headers) || {}, body: init && init.body ? JSON.parse(init.body) : null };
    requests.push(rec);
    const r = responseFor(rec);
    return Promise.resolve({
        ok: r.status < 400,
        status: r.status,
        text: function () { return Promise.resolve(JSON.stringify(r.body)); },
    });
};
const toasts = [];
global.showToast = function (m, k) { toasts.push({ msg: m, kind: k }); };

require('./image-test-page.js');
const Page = window.ImageTestPage;
assert.ok(Page, 'window.ImageTestPage должен быть экспортирован (см. check_iife_exports.py)');
const P = Page.pure;

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
const sleep = function (ms) { return new Promise(function (r) { setTimeout(r, ms); }); };

const BACKENDS = [
    { id: 'imageworker', type: 'image_cpp', host: 'imageworker', imagePort: 18093 },
    { id: 'cppworker-gpu-bundled-agent', type: 'llama_cpp', host: 'cppworker-gpu' },
];

(async function run() {
    // --- 1. видимость --------------------------------------------------------
    check('видимость: страница и пункт меню только при наличии image_cpp', function () {
        assert.strictEqual(P.hasImageBackends([{ type: 'llama_cpp' }]), false);
        assert.strictEqual(P.hasImageBackends(BACKENDS), true);
    });

    // --- 2. сборка тела запроса ---------------------------------------------
    check('buildPayload: размер кратен 64, лимиты шагов/cfg/batch, seed -1 по умолчанию', function () {
        const p = P.buildPayload({ prompt: ' cat ', width: 500, height: 1000, steps: 500, cfg: 99, batch: 99, seed: '', sampler: 'euler_a', scheduler: 'karras' });
        assert.strictEqual(p.prompt, 'cat');
        assert.strictEqual(p.width, 512);
        assert.strictEqual(p.height, 1024);
        assert.strictEqual(p.steps, 100);
        assert.strictEqual(p.cfg_scale, 30);
        assert.strictEqual(p.batch_size, 8);
        assert.strictEqual(p.seed, -1);
    });
    check('payloadForSurface: OpenAI → size/n, A1111 → width/height/batch_size, native → sync', function () {
        const base = P.buildPayload({ prompt: 'x', width: 512, height: 768, steps: 8, cfg: 7, batch: 2, seed: 42, sampler: 'euler', scheduler: 'discrete' });
        const oa = P.payloadForSurface(base, 'openai');
        assert.strictEqual(oa.size, '512x768');
        assert.strictEqual(oa.n, 2);
        assert.strictEqual(oa.width, undefined, 'OpenAI-поверхность не принимает width');
        const a1111 = P.payloadForSurface(base, 'a1111');
        assert.strictEqual(a1111.width, 512);
        assert.strictEqual(a1111.batch_size, 2);
        assert.strictEqual(a1111.size, undefined);
        const nat = P.payloadForSurface(base, 'native');
        assert.strictEqual(nat.sync, true);
        assert.strictEqual(nat.cfg, 7);
    });
    check('surfaceInfo: клиентские URL на портах балансера, не воркера', function () {
        const oa = P.surfaceInfo('openai', '', 'legion.host', 18079, 18080);
        assert.strictEqual(oa.url, '/v1/images/generations');
        assert.ok(oa.externalUrl.indexOf(':18079/v1/images/generations') !== -1, oa.externalUrl);
        const a1111 = P.surfaceInfo('a1111', '', 'legion.host', 18079, 18080);
        assert.ok(a1111.externalUrl.indexOf(':18079/sdapi/v1/txt2img') !== -1, a1111.externalUrl);
        const nat = P.surfaceInfo('native', '', 'legion.host', 18079, 18080);
        assert.ok(nat.externalUrl.indexOf(':18080/api/image/generate') !== -1, nat.externalUrl);
    });
    check('applyModelDefaults: настройки профиля модели подставляются в форму', function () {
        const next = P.applyModelDefaults({ prompt: 'keep me' }, { defaults: { steps: 20, cfgScale: 4, width: 768, height: 512, sampler: 'dpm++2m', scheduler: 'karras', negativePrompt: 'blur' } });
        assert.strictEqual(next.prompt, 'keep me');
        assert.strictEqual(next.steps, 20);
        assert.strictEqual(next.cfg, 4);
        assert.strictEqual(next.width, 768);
        assert.strictEqual(next.sampler, 'dpm++2m');
        assert.strictEqual(next.negative, 'blur');
    });
    check('extractImages: OpenAI/A1111/нативный формы ответа', function () {
        assert.strictEqual(P.extractImages({ data: [{ b64_json: 'AAA' }] }).length, 1);
        assert.strictEqual(P.extractImages({ images: ['BBB'] }).length, 1);
        assert.strictEqual(P.extractImages([{ b64: 'CCC' }]).length, 1);
        assert.strictEqual(P.extractImages({}).length, 0);
    });
    check('normalizeCapabilities: samplers/schedulers/лимиты из воркера, иначе дефолты', function () {
        const caps = P.normalizeCapabilities({ samplers: ['euler'], schedulers: ['karras'], limits: { min_width: 64, max_width: 2048, size_multiple: 64, min_steps: 1, max_steps: 50, max_batch_count: 4 } });
        assert.deepStrictEqual(caps.samplers, ['euler']);
        assert.strictEqual(caps.limits.maxSide, 2048);
        assert.strictEqual(caps.limits.maxSteps, 50);
        const fallback = P.normalizeCapabilities(null);
        assert.ok(fallback.samplers.indexOf('euler_a') !== -1);
        assert.strictEqual(fallback.limits.maxBatch > 0, true);
    });
    check('errorHint: «get sd version from file failed» объясняется как ComfyUI-формат', function () {
        const hint = P.errorHint('sd-server failed ... [ERROR] diffusion_engine.cpp:992 - get sd version from file failed');
        assert.ok(/ComfyUI/.test(hint), 'нет подсказки про ComfyUI: ' + hint);
        assert.ok(/no image model is loaded/i.test(P.errorHint('no image model is loaded on the image backend')) === false);
        assert.ok(/VRAM/.test(P.errorHint('cuda out of memory')), 'нет подсказки про VRAM');
        assert.ok(/Загрузить модель/.test(P.errorHint('no image model is loaded')), 'нет подсказки про загрузку модели');
        assert.strictEqual(P.errorHint(''), '');
    });

    // --- 3. рендер и запрос --------------------------------------------------
    Page.render(BACKENDS);
    getEl('imgTestBackendSelect').value = 'imageworker';
    getEl('imgTestModelSelect').value = 'sd15-q4';
    getEl('imgTestPrompt').value = 'a cat';
    getEl('imgTestWidth').value = 512;
    getEl('imgTestHeight').value = 512;
    getEl('imgTestSteps').value = 8;
    getEl('imgTestCfg').value = 7;
    getEl('imgTestBatch').value = 1;
    getEl('imgTestSurface').value = 'openai';

    check('select бэкендов: только image_cpp', function () {
        const html = getEl('imgTestBackendSelect').innerHTML;
        assert.ok(html.indexOf('imageworker') !== -1, html);
        assert.strictEqual(html.indexOf('cppworker'), -1, 'текстовый бэкенд попал в селект: ' + html);
    });

    requests.length = 0;
    responseFor = function (rec) {
        if (rec.url.indexOf('/models') !== -1) {
            return { status: 200, body: { models: [{ name: 'sd15-q4', state: 'not_loaded', family: 'sd15', defaults: { steps: 8 } }], state: 'not_loaded' } };
        }
        if (rec.url.indexOf('/capabilities') !== -1) {
            return { status: 200, body: { samplers: ['euler_a'], schedulers: ['discrete'], limits: { max_width: 2048 } } };
        }
        if (rec.url.indexOf('/v1/images/generations') !== -1) {
            return { status: 200, body: { created: 1, model: 'sd15-q4', seed: 777, data: [{ b64_json: 'UE5H', output_format: 'png' }] } };
        }
        return { status: 404, body: { error: 'unexpected ' + rec.url } };
    };
    Page._state.backends = BACKENDS;
    const okGen = await Page._actions.generate();
    await sleep(10);

    check('генерация уходит КЛИЕНТСКИМ путём /v1/images/generations (не в управляющий алиас)', function () {
        assert.strictEqual(okGen, true, 'генерация не прошла');
        const gen = requests.filter(function (r) { return r.method === 'POST' && r.url.indexOf('/v1/images/generations') !== -1; })[0];
        assert.ok(gen, 'запрос генерации не ушёл: ' + JSON.stringify(requests.map(function (r) { return r.method + ' ' + r.url; })));
        assert.strictEqual(gen.url.indexOf('/api/v1/image/backends/'), -1, 'запрос ушёл в управляющий алиас: ' + gen.url);
        assert.strictEqual(gen.body.size, '512x512');
        assert.strictEqual(gen.body.steps, 8);
        assert.strictEqual(gen.body.n, 1);
        assert.strictEqual(gen.headers['X-API-Token'], 'test-key-1', 'нет токена админки в заголовках');
    });
    check('результат: картинка отрисована как <img>, есть метаданные (время/seed/model)', function () {
        const html = getEl('imgTestResultHost').innerHTML;
        assert.ok(html.indexOf('<img') !== -1, 'нет <img> с результатом: ' + html.slice(0, 200));
        assert.ok(html.indexOf('data:image/png;base64,UE5H') !== -1, 'нет data URL из b64: ' + html.slice(0, 200));
        const meta = getEl('imgTestResultMeta').textContent;
        assert.ok(meta.indexOf('seed: 777') !== -1, 'нет seed в метаданных: ' + meta);
        assert.ok(meta.indexOf('sd15-q4') !== -1, 'нет модели в метаданных: ' + meta);
        assert.ok(/мс/.test(meta), 'нет времени в метаданных: ' + meta);
    });
    check('история: запись появилась и «В форму» возвращает параметры', function () {
        assert.strictEqual(Page._state.history.length, 1, 'история пуста');
        assert.ok(Page._state.history[0].params.prompt === 'a cat');
        getEl('imgTestPrompt').value = 'changed';
        assert.strictEqual(Page._actions.restoreHistory(0), true);
        assert.strictEqual(getEl('imgTestPrompt').value, 'a cat', 'параметры не вернулись в форму');
    });

    // --- 4. ошибка движка ----------------------------------------------------
    responseFor = function (rec) {
        if (rec.url.indexOf('/models') !== -1) return { status: 200, body: { models: [], state: 'not_loaded' } };
        if (rec.url.indexOf('/capabilities') !== -1) return { status: 404, body: { error: 'no capabilities' } };
        if (rec.url.indexOf('/v1/images/generations') !== -1) {
            return { status: 500, body: { error: { message: 'sd-server failed ... get sd version from file failed', code: 'image_backend_error' }, hint: 'check the bundle' } };
        }
        return { status: 404, body: {} };
    };
    await Page._actions.generate();
    await sleep(10);
    check('ошибка: текст движка + человеческая подсказка в блоке результата', function () {
        const html = getEl('imgTestResultHost').innerHTML;
        assert.ok(html.indexOf('Генерация не удалась') !== -1, 'нет заголовка ошибки: ' + html.slice(0, 200));
        assert.ok(html.indexOf('get sd version from file failed') !== -1, 'нет текста движка');
        assert.ok(html.indexOf('ComfyUI') !== -1, 'нет подсказки про ComfyUI-формат');
    });

    // --- 5. запрос без промпта не уходит ------------------------------------
    requests.length = 0;
    getEl('imgTestPrompt').value = '';
    const okEmpty = await Page._actions.generate();
    check('пустой промпт: запрос не уходит', function () {
        assert.strictEqual(okEmpty, false);
        assert.strictEqual(requests.filter(function (r) { return r.url.indexOf('/v1/images/generations') !== -1; }).length, 0);
    });

    console.log('');
    if (failures.length) {
        console.log('ИТОГ: есть провалы (' + failures.length + ')');
        process.exit(1);
    }
    console.log('ИТОГ: все проверки пройдены (' + passed + ')');
    process.exit(0);
})();
