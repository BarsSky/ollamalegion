// reexport-bundle-phase4.test.js — Фаза 4: шаг «Собрать image-bundle» в блоке
// «Ре-экспорт GGUF» (страница моделей).
//
// ЧТО ЗАКРЕПЛЯЕТСЯ И ПОЧЕМУ.
//
// 1. ШАГ ЕСТЬ И НЕ ЛОМАЕТ СЦЕНАРИЙ D11. Блок остаётся одним сценарием: шаг 6
//    (data-step="bundle") идёт ПОСЛЕ шага 5 и не трогает шаги 1–5 — их порядок и
//    смысл проверяет reexport-scenario-d11.test.js, а здесь проверяется, что
//    новый шаг добавлен ПОСЛЕДНИМ, а не вклинился в середину.
// 2. ПРОВОД, А НЕ ВНЕШНИЙ ВИД. Тела запросов сверяются с контрактом сервиса
//    (services/gguf-reexport): цель `image_bundle`, «проверить» = options.dry_run,
//    «собрать» = та же цель без dry_run, поля — model_dir/name. Сервис
//    разбирается с DisallowUnknownFields, поэтому лишнее поле дало бы 400.
// 3. ИТОГ ГОВОРИТ ПРО РОЛИ И ВЕРДИКТ. Ответ задачи (facts.roles, compat_verdict,
//    worker_checked) обязан доходить до оператора: иначе «bundle собран» ничего
//    не сообщает о том, что именно сервис понял про набор.
// 4. ТЕКСТЫ — ИЗ БОЕВЫХ СЛОВАРЕЙ. i18n грузится из webui/js/i18n/ru.js и en.js:
//    ключ, забытый в одном языке, краснеет здесь (и в ратчете i18n).
//
// Запуск: node webui/tests/reexport-bundle-phase4.test.js
'use strict';

const assert = require('assert');

let checks = 0;
const failures = [];
function check(name, fn) {
    try {
        fn();
        checks++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' — ' + (e && e.message));
    }
}
const asyncChecks = [];
function checkAsync(name, fn) { asyncChecks.push([name, fn]); }

// --- окружение: тот же минимальный DOM, что у D11-набора --------------------
global.window = global;
global.Utils = {
    escapeHtml(s) {
        return String(s === null || s === undefined ? '' : s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }
};

require('../js/i18n/ru.js');
require('../js/i18n/en.js');
const RU = global.window.I18N_RU;
const EN = global.window.I18N_EN;

function translate(pack, key, vars) {
    let s = pack[key];
    if (s === undefined || s === null) s = key;
    if (vars && typeof vars === 'object') {
        Object.keys(vars).forEach(function (p) {
            s = s.split('{' + p + '}').join(String(vars[p]));
        });
    }
    return s;
}
global.I18N = { t: (key, vars) => translate(RU, key, vars) };

function makeEl(sel) {
    return {
        id: String(sel || '').replace('#', ''),
        innerHTML: '',
        textContent: '',
        value: '',
        checked: false,
        disabled: false,
        style: {},
        _listeners: {},
        _attrs: {},
        addEventListener(type, fn) { (this._listeners[type] = this._listeners[type] || []).push(fn); },
        removeEventListener() {},
        setAttribute(k, v) { this._attrs[k] = v; },
        getAttribute(k) { return Object.prototype.hasOwnProperty.call(this._attrs, k) ? this._attrs[k] : null; },
        querySelector(s) { return node(s); },
        querySelectorAll() { return []; },
        contains() { return false; },
        closest() { return null; }
    };
}
const nodes = {};
function node(sel) {
    const key = String(sel);
    if (!nodes[key]) nodes[key] = makeEl(key);
    return nodes[key];
}
const hostEl = makeEl('#ggufReexportHost');
hostEl.querySelector = function (sel) { return node(sel); };

global.document = {
    getElementById(id) { return id === 'ggufReexportHost' ? hostEl : null; },
    addEventListener() {},
    hidden: false
};

// --- сеть: журнал вызовов ---------------------------------------------------
const calls = [];
let fetchImpl = null;
global.fetch = function (url, opts) {
    const call = { url: String(url), opts: opts || {} };
    calls.push(call);
    return fetchImpl(call);
};
function jsonResponse(status, body) {
    return Promise.resolve({
        ok: status >= 200 && status < 300,
        status,
        text() { return Promise.resolve(body === undefined ? '' : JSON.stringify(body)); }
    });
}

require('../js/modules/gguf-reexport-jobs.js');
require('../js/modules/gguf-reexport.js');
const R = global.window.GgufReexport;
const wait = (ms) => new Promise((r) => setTimeout(r, ms));

const CAPS = {
    service: 'gguf-reexport',
    available: true,
    version: { build_version: 'r261121-r732' },
    converter: { name: 'llama.cpp', revision: '5bacb7c868', python: '3.12.16', torch_state: 'absent' },
    architectures: [],
    targets: [
        { id: 'inkling2_loop', label: 'Inkling-2 loop', kind: 'metadata_patch', available: true, options: [] },
        { id: 'hf_to_gguf', label: 'HF → GGUF', kind: 'convert', available: false, reason: 'нет torch' },
        {
            id: 'image_bundle', label: 'Набор файлов → image-bundle', kind: 'bundle', available: true,
            options: [{ id: 'dry_run', type: 'bool', default: false }]
        }
    ],
    limits: { max_concurrent: 1, models_dir: '/app/models' }
};

const MODELS = { models: [] };

// Задача сборки: ровно те факты, что отдаёт сервис (см. bundle.go: bundleFacts).
const BUNDLE_JOB_OK = {
    id: 'rj-bundle-1', status: 'succeeded', target: 'image_bundle',
    model_dir: 'image/my-model',
    out_path: '/app/models/image/my-model',
    progress: { phase: 'done', stage: 'done', percent: 100 },
    result: {
        duration_ms: 5200,
        src_bytes: 100,
        dst_bytes: 100,
        facts: {
            bundle: 'my-model', bundle_path: '/app/models/image/my-model',
            in_place: false, roles_source: 'headers', family: 'flux', files: 2,
            roles: { 'flux1-q4.gguf': 'diffusion', 'ae.safetensors': 'vae' },
            compat_verdict: 'ready', compat_reason: 'движок запустит набор как DiT',
            engine_mode: 'diffusion-model', ready: true, worker_checked: true,
            worker_verdict: 'ready'
        },
        verify: { compat_verdict: 'ready', ready: true, worker_checked: true }
    }
};

function standardFetch(overrides) {
    const o = overrides || {};
    return (call) => {
        if (call.url.indexOf('/capabilities') >= 0) return o.caps ? o.caps(call) : jsonResponse(200, CAPS);
        if (call.url.indexOf('/models') >= 0 && call.opts.method !== 'POST') {
            return o.models ? o.models(call) : jsonResponse(200, MODELS);
        }
        if (call.url.indexOf('/jobs') >= 0) {
            if (call.opts.method === 'POST') return o.create ? o.create(call) : jsonResponse(202, BUNDLE_JOB_OK);
            if (o.job) return o.job(call);
            return jsonResponse(200, { jobs: [] });
        }
        return jsonResponse(404, {});
    };
}

// --- синхронные проверки ---------------------------------------------------

check('шаг сборки bundle: есть в разметке и идёт ПОСЛЕ проверки загрузки', function () {
    const html = R._buildHTML();
    const bundle = html.indexOf('data-step="bundle"');
    const load = html.indexOf('data-step="load"');
    const normalize = html.indexOf('data-step="normalize"');
    assert.ok(bundle >= 0, 'нет шага data-step="bundle" — оператор не найдёт сборку набора');
    assert.ok(load >= 0 && normalize >= 0, 'пропали шаги D11: сценарий сломан');
    assert.ok(bundle > load, 'шаг сборки вклинился перед шагами D11 (порядок сценария нарушен)');
    // Поля шага: каталог набора и имя bundle.
    assert.ok(html.indexOf('id="rxBundleDir"') >= 0, 'нет поля каталога набора');
    assert.ok(html.indexOf('id="rxBundleName"') >= 0, 'нет поля имени bundle');
});

check('тело задачи: цель image_bundle, dry_run только у проверки, имя из формы', function () {
    const el = makeEl('#ggufReexportHost');
    node('#rxBundleDir').value = 'image/my-model';
    node('#rxBundleName').value = '  built-name  ';
    const checkBody = R._buildBundleBody(el, true);
    assert.strictEqual(checkBody.target, 'image_bundle');
    assert.strictEqual(checkBody.model_dir, 'image/my-model');
    assert.strictEqual(checkBody.options.dry_run, true, '«Проверить набор» обязана уходить с dry_run');
    assert.strictEqual(checkBody.options.name, 'built-name', 'имя не обрезано/не передано');
    const buildBody = R._buildBundleBody(el, false);
    assert.strictEqual(buildBody.options.dry_run, undefined,
        '«Собрать bundle» не должна нести dry_run: сервис ничего бы не записал');
    assert.strictEqual(buildBody.target, 'image_bundle');
});

check('i18n: ключи шага есть в ru и en, значения непустые', function () {
    const keys = Object.keys(RU).filter((k) => k.indexOf('reexport.bundle_') === 0).sort();
    assert.ok(keys.length >= 10, 'ключей шага слишком мало: ' + keys.length);
    keys.forEach((k) => {
        assert.ok(RU[k] && RU[k].trim().length > 0, 'пустой ru-перевод: ' + k);
        assert.ok(EN[k] && EN[k].trim().length > 0, 'нет en-перевода: ' + k);
    });
    ['reexport.step_bundle_title', 'reexport.btn_bundle_check', 'reexport.btn_bundle_build'].forEach((k) => {
        assert.ok(RU[k] && EN[k], 'нет ключа ' + k + ' в одном из словарей');
    });
});

check('недоступная цель: кнопки выключены и причина названа', function () {
    R._state.caps = { targets: [{ id: 'image_bundle', kind: 'bundle', available: false, reason: 'нет возможности' }] };
    R._state.bundle = null;
    R._renderBundle(makeEl('#ggufReexportHost'));
    assert.strictEqual(node('#rxBundleCheck').disabled, true, 'кнопка проверки активна при недоступной цели');
    assert.strictEqual(node('#rxBundleBuild').disabled, true, 'кнопка сборки активна при недоступной цели');
    assert.ok(node('#rxBundleStatus').textContent.indexOf('нет возможности') >= 0,
        'причина недоступности не показана: ' + node('#rxBundleStatus').textContent);
});

check('итог: роли, семейство и вердикт видны оператору', function () {
    R._state.caps = CAPS;
    R._state.bundle = { status: 'succeeded', jobId: 'rj-bundle-1', job: BUNDLE_JOB_OK, error: '', dryRun: false };
    R._renderBundle(makeEl('#ggufReexportHost'));
    const status = node('#rxBundleStatus').textContent;
    assert.ok(status.indexOf('bundle собран') >= 0, 'статус итога не показан: ' + status);
    const html = node('#rxBundleResult').innerHTML;
    assert.ok(html.indexOf('flux') >= 0, 'семейство не показано: ' + html);
    assert.ok(html.indexOf('ready') >= 0, 'вердикт не показан: ' + html);
    assert.ok(html.indexOf('flux1-q4.gguf') >= 0 && html.indexOf('diffusion') >= 0,
        'роли файлов не показаны: ' + html);
    assert.ok(html.indexOf('проверено воркером') >= 0, 'отметка проверки воркером потеряна: ' + html);
});

// --- асинхронные проверки --------------------------------------------------

checkAsync('сборка: POST с целью image_bundle, ожидание статуса и итог на экране', async () => {
    // job: термальный статус — иначе ожидание задачи не закончится (поллинг
    // честно ждёт терминального состояния, и тест висел бы на нём).
    fetchImpl = standardFetch({ job: () => jsonResponse(200, BUNDLE_JOB_OK) });
    calls.length = 0;
    const ok = await R.mount();
    await wait(30);
    assert.strictEqual(ok, true, 'блок не поднялся при живых capabilities');
    node('#rxBundleDir').value = 'image/my-model';
    node('#rxBundleName').value = 'my-model';
    await R._runBundle(hostEl, false);
    const post = calls.filter((c) => c.url.indexOf('/jobs') >= 0 && c.opts.method === 'POST');
    assert.strictEqual(post.length, 1, 'ожидался один POST задачи, получено ' + post.length);
    const body = JSON.parse(post[0].opts.body);
    assert.strictEqual(body.target, 'image_bundle');
    assert.strictEqual(body.model_dir, 'image/my-model');
    assert.strictEqual(body.options.dry_run, undefined, 'сборка ушла как пробный прогон');
    assert.strictEqual(R._state.bundle.status, 'succeeded');
    assert.ok(node('#rxBundleResult').innerHTML.indexOf('ready') >= 0, 'итог не отрисован после сборки');
    R._stopPolling();
});

checkAsync('без слоя задач шаг честно говорит об этом, а не молчит', async () => {
    const saved = global.window.GgufReexportJobs;
    delete global.window.GgufReexportJobs;
    R._resetJobsLayer();
    fetchImpl = standardFetch({ job: () => jsonResponse(200, BUNDLE_JOB_OK) });
    await R.mount();
    await wait(20);
    node('#rxBundleDir').value = 'image/my-model';
    await R._runBundle(hostEl, false);
    assert.ok(node('#rxError').textContent.indexOf('Слой задач не загружен') >= 0,
        'нет честного сообщения об отсутствии слоя задач: ' + node('#rxError').textContent);
    global.window.GgufReexportJobs = saved;
    R._resetJobsLayer();
    R._stopPolling();
});

(async () => {
    for (const [name, fn] of asyncChecks) {
        try {
            await fn();
            checks++;
            console.log('  \u2713 ' + name);
        } catch (e) {
            failures.push(name + ': ' + (e && e.message));
            console.error('  \u2717 ' + name + ' — ' + (e && e.message));
        }
    }
    R._stopPolling();
    console.log('\n' + checks + ' проверок зелёные, ' + failures.length + ' красных');
    if (failures.length > 0) {
        failures.forEach(function (f) { console.error('  FAIL: ' + f); });
        process.exit(1);
    }
})();
