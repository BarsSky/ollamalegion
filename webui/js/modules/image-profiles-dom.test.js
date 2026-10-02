// image-profiles-dom.test.js - R-Image Phase 6 (2026-10-02): интеграционный
// smoke редактора профилей image-моделей с мок-DOM и мок-fetch (без браузера).
//
// Запуск: node webui/js/modules/image-profiles-dom.test.js
//
// Что проверяем (то, что не поймать чистыми функциями):
//   1. Каждый статический DOM-id, за которым модуль ходит через
//      document.getElementById, реально есть в webui/index.html (ловит
//      рассинхрон разметки и JS - «секция молча не работает»).
//   2. Каждый id, который модуль генерирует в модалке редактора
//      (id="imgp..."), присутствует в исходнике модуля (id есть, и за ним
//      можно ходить).
//   3. Каждый i18n-ключ image.profiles.*, который дёргает модуль, есть в ru.js.
//   4. mount()/refresh(): GET список профилей + каталог, отрисовка строки
//      с кнопками Edit/Duplicate/Apply/Delete.
//   5. Клик по Apply -> POST .../apply, состояние «применён» у строки,
//      недоступный воркер -> честный статус unreachable (НЕ «успех»).
//   6. Клик по Delete -> DELETE + перезагрузка списка.
//   7. Ошибка API списка показывается текстом (а не пустой таблицей).

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
        console.error('  \u2717 ' + name + ' - ' + (e && e.message));
    }
}

// --- mocks: window / localStorage / i18n -----------------------------------
global.window = global;
global.addEventListener = function () {};
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
global.Api = {
    getAuthHeaders: function () {
        return { 'Content-Type': 'application/json', 'X-API-Token': 'test-token' };
    },
};
global.WEBUI_CONFIG = { API_BASE: 'http://balancer.test:18081', API_TOKEN: 'test-token' };
global.confirm = function () { return confirmAnswer; };
let confirmAnswer = true;

// --- inventory: id модуля / index.html ------------------------------------
const MODULE_SRC = fs.readFileSync(path.join(__dirname, 'image-profiles.js'), 'utf8');
const INDEX_HTML = fs.readFileSync(path.join(__dirname, '..', '..', 'index.html'), 'utf8');
const RU_SRC = fs.readFileSync(path.join(__dirname, '..', 'i18n', 'ru.js'), 'utf8');

const HTML_IDS = new Set();
{
    const re = /id="([A-Za-z0-9_-]+)"/g;
    let m;
    while ((m = re.exec(INDEX_HTML)) !== null) HTML_IDS.add(m[1]);
}

// id, которые модуль создаёт САМ в модалке редактора (их нет в index.html).
const GENERATED_EDITOR_IDS = new Set();
{
    const re = /\bid="(imgp[A-Za-z0-9]*)"/g;
    let m;
    while ((m = re.exec(MODULE_SRC)) !== null) GENERATED_EDITOR_IDS.add(m[1]);
}

// Статические id, за которыми модуль ходит через getElementById.
// В модуле два пути: локальный хелпер el('id') и прямой document.getElementById.
const STATIC_IDS = new Set();
{
    const patterns = [
        /getElementById\('([A-Za-z0-9_-]+)'\)/g,
        /\bel\('([A-Za-z0-9_-]+)'\)/g,
    ];
    patterns.forEach(function (re) {
        let m;
        while ((m = re.exec(MODULE_SRC)) !== null) {
            if (!GENERATED_EDITOR_IDS.has(m[1])) STATIC_IDS.add(m[1]);
        }
    });
}

// --- mocks: DOM ------------------------------------------------------------
const elements = {};
const accessedIds = [];
function makeElement(id) {
    const el = {
        id: id,
        style: {},
        _html: '',
        textContent: '',
        value: '',
        checked: false,
        disabled: false,
        _listeners: {},
        _attrs: {},
        addEventListener: function (type, fn) { (this._listeners[type] = this._listeners[type] || []).push(fn); },
        removeEventListener: function () {},
        dispatch: function (type, ev) {
            (this._listeners[type] || []).forEach(function (fn) { fn(ev || {}); });
        },
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
function getEl(id) {
    if (!Object.prototype.hasOwnProperty.call(elements, id)) {
        accessedIds.push(id);
        elements[id] = makeElement(id);
    }
    return elements[id];
}
global.document = {
    getElementById: getEl,
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

const PROFILES = {
    models: {
        'sd15-q8': {
            name: 'sd15-q8', family: 'sd15',
            files: [{ role: 'diffusion', repo: 'second-state/stable-diffusion-v1-5-GGUF', filename: 'sd15-Q8_0.gguf', sizeBytes: 1763578176, localPath: '/models/sd15-q8/sd15-Q8_0.gguf' }],
            defaults: { steps: 25, cfgScale: 7, sampler: 'euler_a', scheduler: 'discrete', width: 512, height: 512, batchCount: 1, seed: -1 },
            runtime: { vaeTiling: true, seedMode: 'random' },
            vramEstimateMb: 2100,
            timeoutSec: 300,
            idleUnloadMinutes: 15,
            notes: 'эталон SD1.5'
        },
        'flux-disabled': {
            name: 'flux-disabled', family: 'flux', disabled: true,
            files: [
                { role: 'diffusion', repo: 'leejet/FLUX.1-schnell-gguf', filename: 'flux1-schnell-Q4_0.gguf', sizeBytes: 6800000000 },
                { role: 'vae', repo: 'black-forest-labs/FLUX.1-schnell', filename: 'ae.safetensors' }
            ],
            defaults: { steps: 4, cfgScale: 1, sampler: 'euler', scheduler: 'simple', width: 1024, height: 1024, batchCount: 1, seed: -1 },
            runtime: { seedMode: 'random' },
            vramEstimateMb: 7000
        }
    },
    total: 2
};

const CATALOG = {
    version: 1,
    presets: [
        { name: 'sd15-q4-0', family: 'sd15', files: [{ role: 'diffusion', repo: 'r', filename: 'f.gguf' }] },
        { name: 'z-image-turbo-q3k', family: 'z_image', files: [{ role: 'diffusion', repo: 'leejet/Z-Image-Turbo-GGUF', filename: 'z.gguf' }, { role: 'vae', repo: 'r', filename: 'v.safetensors' }] }
    ],
    total: 2
};

let listStatus = 200;
let applyBackends = [{ backendId: 'img-1', status: 'persisted', message: 'saved' }];
let deleteStatus = 200;

global.fetch = function (url, init) {
    const method = (init && init.method) || 'GET';
    const u = String(url);
    const p = u.replace(/^https?:\/\/[^/]+/, '').split('?')[0];
    if (p === '/v1/images/generations') {
        // image-page.js не грузим, но на всякий случай.
        return jsonResponse({});
    }
    requests.push({ url: u, path: p, method: method, body: init && init.body ? JSON.parse(init.body) : null, headers: (init && init.headers) || {} });
    if (p === '/api/v1/image/model-profiles' && method === 'GET') {
        if (listStatus !== 200) return jsonResponse({ error: 'failed to load image model profiles', message: 'boom' }, listStatus);
        return jsonResponse(PROFILES);
    }
    if (p === '/api/v1/image/model-profiles/sd15-q8' && method === 'GET') {
        const prof = PROFILES.models['sd15-q8'];
        return jsonResponse({ model: 'sd15-q8', profile: prof, serverArgs: ['--model', '/models/sd15-q8/sd15-Q8_0.gguf', '--vae-tiling'] });
    }
    if (p === '/api/v1/image/model-profiles/sd15-q8' && method === 'DELETE') {
        if (deleteStatus !== 200) return jsonResponse({ error: 'profile not found' }, deleteStatus);
        return jsonResponse({ status: 'ok', model: 'sd15-q8' });
    }
    if (p === '/api/v1/image/model-profiles/sd15-q8/apply' && method === 'POST') {
        return jsonResponse({
            model: 'sd15-q8', applyId: 'imgapply-1', status: 'completed',
            backends: applyBackends,
        });
    }
    if (p === '/api/v1/image/model-profiles/flux-disabled/apply' && method === 'POST') {
        return jsonResponse({
            model: 'flux-disabled', applyId: 'imgapply-2', status: 'completed',
            backends: [{ backendId: 'img-1', status: 'unreachable', message: 'image worker 10.0.0.2:18093: dial tcp: connection refused' }],
        });
    }
    if (p === '/api/v1/image/model-catalog') return jsonResponse(CATALOG);
    if (p.indexOf('/api/v1/image/model-profiles/') === 0 && (method === 'PUT' || method === 'POST')) {
        // Upsert/apply любого профиля: тело эхо-возвращаем, как это делает сервер.
        return jsonResponse({ status: 'ok', model: p.split('/')[5], profile: (init && init.body) ? JSON.parse(init.body) : {} });
    }
    if (p === '/api/v1/image/backends') return jsonResponse({ backends: [{ id: 'img-1', name: 'GPU image', type: 'image_cpp', host: '10.0.0.2', imagePort: 18093, status: 'healthy' }] });
    if (p === '/api/v1/backends') return jsonResponse({ backends: [{ id: 'llama-1', type: 'llama_cpp' }, { id: 'img-1', type: 'image_cpp' }] });
    return jsonResponse({ error: 'unexpected ' + p }, 404);
};

require('./image-profiles.js');
const IP = global.ImageProfiles;
assert.ok(IP, 'window.ImageProfiles должен быть экспортирован');

const sleep = function (ms) { return new Promise(function (r) { setTimeout(r, ms); }); };

/** Синтетический клик по [data-imgp-action] внутри делегированного контейнера. */
function clickAction(container, action, name) {
    const target = makeElement('click-target');
    target._attrs['data-imgp-action'] = action;
    if (name !== undefined) target._attrs['data-name'] = name;
    target.closest = function (sel) {
        if (sel === '[data-imgp-action]') return this;
        return null;
    };
    container.dispatch('click', { target: target });
}

/**
 * Мини-документ формы редактора: значения полей по data-imgp-field, чекбоксы
 * и строки файлов. Возвращает doc, который модуль принимает как #imgpEditorOverlay.
 * Побочный эффект: id служебных узлов (imgpErrors/imgpFileRows/imgpServerArgs)
 * регистрируются в мок-document, чтобы getElementById нашёл их.
 */
function makeFakeEditor(fields, checks, rows) {
    const nodes = [];
    function node(id, type, value, checked) {
        const obj = {
            attr: id, type: type, value: value === undefined ? '' : String(value), checked: !!checked,
            getAttribute: function (n) { return n === 'data-imgp-field' ? id : null; },
        };
        nodes.push(obj);
        return obj;
    }
    Object.keys(fields || {}).forEach(function (id) { node(id, 'text', fields[id]); });
    Object.keys(checks || {}).forEach(function (id) { node(id, 'checkbox', '', checks[id]); });
    const fileRows = (rows || []).map(function (r, i) {
        return {
            attr: String(i),
            querySelector: function (sel) {
                const m = /^\[data-imgp-file="([a-z]+)"\]$/.exec(sel);
                if (!m) return null;
                return { value: r[m[1]] === undefined ? '' : String(r[m[1]]) };
            }
        };
    });
    // Служебные узлы модалки.
    elements['imgpErrors'] = makeElement('imgpErrors');
    elements['imgpServerArgs'] = makeElement('imgpServerArgs');
    elements['imgpFileRows'] = makeElement('imgpFileRows');
    return {
        querySelectorAll: function (sel) {
            if (sel === '[data-imgp-file-row]') return fileRows;
            if (sel === '[data-imgp-field]') return nodes;
            return [];
        },
        querySelector: function (sel) {
            const m = /^\[data-imgp-field="([^"]+)"\]$/.exec(sel);
            if (!m) return null;
            return nodes.filter(function (n) { return n.attr === m[1]; })[0] || null;
        }
    };
}

(async function run() {
    console.log('image-profiles DOM smoke');

    // --- 1/2/3. Статическая сверка JS <-> HTML <-> i18n -------------------
    check('все статические DOM-id модуля есть в webui/index.html', function () {
        const missing = Array.from(STATIC_IDS).filter(function (id) { return !HTML_IDS.has(id); });
        assert.deepStrictEqual(missing, [], 'нет в index.html: ' + missing.join(', '));
        assert.ok(STATIC_IDS.size >= 6, 'ожидалось несколько id, найдено ' + STATIC_IDS.size);
    });
    check('id модалки редактора генерируются модулем (id="imgp..." есть в исходнике)', function () {
        const required = ['imgpEditorOverlay', 'imgpErrors', 'imgpName', 'imgpFamily', 'imgpFileRows', 'imgpServerArgs', 'imgpSave', 'imgpSaveApply', 'imgpCancel', 'imgpPreset'];
        const missing = required.filter(function (id) { return !GENERATED_EDITOR_IDS.has(id); });
        assert.deepStrictEqual(missing, [], 'нет в разметке редактора: ' + missing.join(', '));
        // И наоборот: каждый data-imgp-field, который читает formStateFromDom,
        // должен иметь соответствующий id в разметке.
        const fields = Array.from(MODULE_SRC.matchAll(/data-imgp-field="([A-Za-z0-9_]+)"/g)).map(function (m) { return m[1]; });
        assert.ok(fields.length >= 20, 'ожидалось >= 20 полей формы, найдено ' + fields.length);
        const orphan = fields.filter(function (f) { return !GENERATED_EDITOR_IDS.has(f); });
        assert.deepStrictEqual(orphan, [], 'data-imgp-field без id в разметке: ' + orphan.join(', '));
    });

    // --- 4. mount / refresh ----------------------------------------------
    const mounted = IP.mount('imageProfiles');
    check('mount("imageProfiles") находит контейнер из index.html', function () {
        assert.strictEqual(mounted, true);
        assert.strictEqual(IP._state.container, elements['imageProfiles']);
        assert.ok(elements['imageProfiles']._listeners['click'], 'делегированный обработчик подписан');
    });

    await IP.refresh();
    await sleep(20);

    check('refresh(): GET список профилей + каталог', function () {
        const list = requests.filter(function (r) { return r.path === '/api/v1/image/model-profiles'; });
        const cat = requests.filter(function (r) { return r.path === '/api/v1/image/model-catalog'; });
        assert.ok(list.length >= 1, 'нет запроса списка профилей');
        assert.ok(cat.length >= 1, 'нет запроса каталога');
        assert.strictEqual(list[0].headers['X-API-Token'], 'test-token', 'заголовок авторизации');
        assert.strictEqual(IP._state.profiles.length, 2);
        assert.strictEqual(IP._state.catalog.length, 2, 'каталог пресетов загружен');
    });

    check('таблица: имя, family, размер bundle, VRAM, disabled и 4 кнопки действий', function () {
        const html = elements['imageProfilesBody'].innerHTML;
        assert.ok(html.indexOf('sd15-q8') !== -1, 'нет имени');
        assert.ok(html.indexOf('sd15') !== -1, 'нет семейства');
        assert.ok(html.indexOf('1.6 GB') !== -1, 'нет размера bundle: ' + html.slice(0, 300));
        assert.ok(html.indexOf('2100 MB') !== -1, 'нет оценки VRAM');
        assert.ok(html.indexOf('flux-disabled') !== -1);
        assert.ok(html.indexOf('disabled') !== -1 || html.indexOf('выключен') !== -1, 'нет бейджа disabled');
        assert.ok(html.indexOf('data-imgp-action="edit"') !== -1, 'нет Edit');
        assert.ok(html.indexOf('data-imgp-action="duplicate"') !== -1, 'нет Duplicate');
        assert.ok(html.indexOf('data-imgp-action="apply"') !== -1, 'нет Apply');
        assert.ok(html.indexOf('data-imgp-action="delete"') !== -1, 'нет Delete');
        assert.ok(html.indexOf('не применён') !== -1, 'нет статуса «не применён»');
    });

    check('счётчик профилей заполнен (i18n {n})', function () {
        assert.strictEqual(elements['imageProfilesCount'].textContent, 'Профилей: 2');
    });

    // --- 4b. сохранение из формы редактора --------------------------------
    check('save: PUT с телом профиля (defaults/runtime/files построены из формы)', async function () {
        const editorDoc = makeFakeEditor({
            imgpName: 'my-new-profile', imgpFamily: 'flux', imgpNotes: 'n',
            imgpVram: '7000', imgpTimeout: '600', imgpIdleUnload: '15',
            imgpSteps: '4', imgpCfg: '1', imgpWidth: '1024', imgpHeight: '1024', imgpBatch: '1',
            imgpSeed: '-1', imgpClipSkip: '0', imgpSampler: 'euler', imgpScheduler: 'simple',
            imgpNegative: 'ugly', imgpRuntimeBackend: 'te=cpu', imgpParamsBackend: '', imgpMaxVram: '6',
            imgpAutoFit: 'on', imgpSplitMode: 'row', imgpGpuLayers: '24', imgpThreads: '6',
            imgpVaeTileSize: '512', imgpSeedMode: 'fixed', imgpExtraArgs: '--foo\n--bar'
        }, { imgpDisabled: false, imgpOffloadToCpu: false, imgpDiffusionFa: true, imgpVaeTiling: true }, [
            { role: 'diffusion', repo: 'leejet/FLUX.1-schnell-gguf', filename: 'q4.gguf', revision: 'main' },
            { role: 'vae', repo: 'black-forest-labs/FLUX.1-schnell', filename: 'ae.safetensors', revision: 'main' }
        ]);
        const res = await IP._actions.save(editorDoc, { apply: false });
        assert.strictEqual(res.ok, true, 'сохранение должно пройти: ' + JSON.stringify(res.errors || res.error));
        const put = requests.filter(function (r) {
            return r.path === '/api/v1/image/model-profiles/my-new-profile' && r.method === 'PUT';
        })[0];
        assert.ok(put, 'PUT не ушёл');
        assert.strictEqual(put.body.family, 'flux');
        assert.strictEqual(put.body.files.length, 2);
        assert.strictEqual(put.body.defaults.steps, 4);
        assert.strictEqual(put.body.defaults.seed, -1);
        assert.strictEqual(put.body.runtime.vaeTiling, true);
        assert.strictEqual(put.body.runtime.diffusionFa, true);
        assert.strictEqual(put.body.runtime.offloadToCpu, false, 'булевы уходят всегда (presence)');
        assert.strictEqual(put.body.runtime.seedMode, 'fixed');
        assert.strictEqual(put.body.runtime.vaeTileSize, 512);
        assert.deepStrictEqual(put.body.runtime.extraArgs, ['--foo', '--bar']);
        assert.strictEqual(put.body.vramEstimateMb, 7000);
        assert.strictEqual(put.body.disabled, false);
        // Файлы: localPath/sizeBytes не уходят (их знает сервер).
        assert.deepStrictEqual(Object.keys(put.body.files[0]).sort(), ['filename', 'repo', 'revision', 'role']);
    });

    check('save: невалидная форма -> PUT не уходит, ошибки показаны', async function () {
        const editorDoc = makeFakeEditor({
            imgpName: 'bad-profile', imgpFamily: 'flux',
            imgpSteps: '4', imgpCfg: '1', imgpWidth: '1000', imgpHeight: '1024', imgpBatch: '1',
            imgpSeed: '-1', imgpClipSkip: '0', imgpSampler: 'euler', imgpScheduler: 'simple',
            imgpSeedMode: 'random', imgpParamsBackend: 'cpu'
        }, { imgpOffloadToCpu: true }, [
            // diffusion есть, но vae для flux не задан; ширина 1000 не кратна 64;
            // offloadToCpu + paramsBackend вместе.
            { role: 'diffusion', repo: 'r', filename: 'q4.gguf', revision: 'main' }
        ]);
        const before = requests.length;
        const res = await IP._actions.save(editorDoc, { apply: true });
        assert.strictEqual(res.ok, false);
        const codes = res.errors.map(function (e) { return e.code; });
        assert.ok(codes.indexOf('bad_side_multiple') !== -1, 'нет ошибки кратности 64');
        assert.ok(codes.indexOf('dit_need_vae') !== -1, 'нет ошибки про обязательный vae');
        assert.ok(codes.indexOf('offload_params_conflict') !== -1, 'нет ошибки взаимоисключающих флагов');
        const writes = requests.filter(function (r) {
            return r.path.indexOf('bad-profile') !== -1 &&
                (r.method === 'PUT' || r.path.indexOf('/apply') !== -1);
        });
        assert.deepStrictEqual(writes, [], 'невалидная форма не должна сохраняться/применяться');
        assert.ok(elements['imgpErrors'].innerHTML.indexOf('Исправьте перед сохранением') !== -1, 'ошибки не показаны в форме');
        assert.strictEqual(elements['imgpErrors'].style.display, '');
        // Тост с человекочитаемым текстом ошибки (i18n-ключ, не код).
        const last = toasts[toasts.length - 1];
        assert.strictEqual(last.type, 'error');
        assert.ok(last.msg.indexOf('image.profiles.err.') === -1, 'показан сырой i18n-ключ: ' + last.msg);
        assert.ok(/vae|кратен 64/.test(last.msg), 'текст ошибки не переведён: ' + last.msg);
        // В самой форме перечислены ВСЕ ошибки, а не только первая.
        const box = elements['imgpErrors'].innerHTML;
        assert.ok(box.indexOf('кратен 64') !== -1, 'в списке ошибок нет кратности 64');
        assert.ok(box.indexOf('взаимоисключающие') !== -1, 'в списке ошибок нет конфликта offload/paramsBackend');
    });

    check('formToProfile не теряет localPath/sizeBytes у неизменившихся строк', function () {
        const form = IP.pure.profileToForm({
            name: 'sd15-q8', family: 'sd15',
            files: [{ role: 'diffusion', repo: 'r', filename: 'f.gguf', revision: 'main', sizeBytes: 123, localPath: '/m/f.gguf' }],
            defaults: { steps: 25, cfgScale: 7, width: 512, height: 512, batchCount: 1, seed: -1 },
            runtime: { seedMode: 'random' }
        }, true);
        const files = IP.pure.filesFromForm(form);
        assert.strictEqual(files.length, 1);
        assert.ok(!('localPath' in files[0]), 'PUT не должен нести localPath (сервер знает его сам)');
    });

    // --- 5. apply: persisted -> применён, unreachable -> честный статус ---
    clickAction(elements['imageProfiles'], 'apply', 'sd15-q8');
    await sleep(30);

    check('Apply: POST .../apply без тела + статус «применён» у строки', function () {
        const applyReq = requests.filter(function (r) { return r.path === '/api/v1/image/model-profiles/sd15-q8/apply'; })[0];
        assert.ok(applyReq, 'POST apply не ушёл');
        assert.strictEqual(applyReq.method, 'POST');
        assert.strictEqual(applyReq.body, null, 'тело пустое: сервер берёт сохранённый профиль');
        const state = IP._state.applied['sd15-q8'];
        assert.strictEqual(state.outcome, 'applied');
        assert.strictEqual(state.applyId, 'imgapply-1');
        assert.ok(elements['imageProfilesBody'].innerHTML.indexOf('применён') !== -1, 'в таблице нет статуса «применён»');
        const ok = toasts.filter(function (t) { return t.msg.indexOf('Профиль применён на воркерах (1)') !== -1; })[0];
        assert.ok(ok, 'нет тоста об успешном применении');
    });

    clickAction(elements['imageProfiles'], 'apply', 'flux-disabled');
    await sleep(30);

    check('Apply при недоступном воркере: статус unreachable, а НЕ «успех»', function () {
        const state = IP._state.applied['flux-disabled'];
        assert.strictEqual(state.outcome, 'unreachable');
        assert.strictEqual(state.unreachable, 1);
        const html = elements['imageProfilesBody'].innerHTML;
        assert.ok(html.indexOf('воркер недоступен') !== -1, 'нет честного статуса unreachable');
        const warn = toasts.filter(function (t) { return t.type === 'warn' && t.msg.indexOf('image-воркер не ответил') !== -1; })[0];
        assert.ok(warn, 'нет warning про недоступный воркер');
        const fake = toasts.filter(function (t) { return t.type === 'success' && t.msg.indexOf('flux-disabled') !== -1; });
        assert.deepStrictEqual(fake, [], 'не должно быть тоста об успехе для недоступного воркера');
    });

    // --- 6. delete --------------------------------------------------------
    const before = requests.length;
    confirmAnswer = true;
    clickAction(elements['imageProfiles'], 'delete', 'sd15-q8');
    await sleep(30);

    check('Delete: confirm -> DELETE + перезагрузка списка + тост', function () {
        const del = requests.slice(before).filter(function (r) { return r.path === '/api/v1/image/model-profiles/sd15-q8' && r.method === 'DELETE'; })[0];
        assert.ok(del, 'DELETE не ушёл');
        const after = requests.slice(before).filter(function (r) { return r.path === '/api/v1/image/model-profiles' && r.method === 'GET'; });
        assert.ok(after.length >= 1, 'список не перечитан после удаления');
        assert.ok(toasts.filter(function (t) { return t.msg.indexOf('Профиль удалён') !== -1; })[0], 'нет тоста удаления');
    });

    check('Delete: отказ в confirm -> запроса нет', async function () {
        confirmAnswer = false;
        const n = requests.length;
        clickAction(elements['imageProfiles'], 'delete', 'flux-disabled');
        await sleep(20);
        const del = requests.slice(n).filter(function (r) { return r.method === 'DELETE'; });
        assert.deepStrictEqual(del, [], 'DELETE не должен уходить без подтверждения');
        confirmAnswer = true;
    });

    // --- 7. ошибка API списка --------------------------------------------
    IP._state.loaded = false;
    listStatus = 500;
    await IP.refresh();
    await sleep(20);

    check('ошибка списка: текст ошибки в notice, таблица не падает', function () {
        const notice = elements['imageProfilesNotice'];
        assert.strictEqual(notice.style.display, '', 'notice должен быть виден');
        assert.ok(notice.innerHTML.indexOf('Не удалось загрузить профили') !== -1, notice.innerHTML);
        assert.ok(elements['imageProfilesBody'].innerHTML.indexOf('Профилей пока нет') !== -1, 'пустое состояние вместо мусора');
        assert.strictEqual(IP._state.profiles.length, 0);
    });
    listStatus = 200;

    // --- 8. доступ к деталям профиля (edit) ------------------------------
    await IP.refresh();
    await sleep(10);
    await IP._actions.edit('sd15-q8');
    await sleep(20);

    check('edit: GET деталей профиля запрашивается (serverArgs для панели флагов)', function () {
        const detail = requests.filter(function (r) { return r.path === '/api/v1/image/model-profiles/sd15-q8' && r.method === 'GET'; });
        assert.ok(detail.length >= 1, 'GET профиля не ушёл');
    });
    IP._actions.closeEditor();

    // --- 9. i18n-ключи модуля --------------------------------------------
    check('все i18n-ключи, которые дёргает модуль, есть в ru.js', function () {
        assert.deepStrictEqual(missingKeys, [], 'нет ключей в ru.js: ' + missingKeys.join(', '));
    });

    check('все data-i18n ключи секции профилей в index.html есть в ru.js', function () {
        const htmlKeys = Array.from(INDEX_HTML.matchAll(/data-i18n="(image\.profiles\.[A-Za-z0-9_.]+)"/g)).map(function (m) { return m[1]; });
        assert.ok(htmlKeys.length >= 8, 'ожидались ключи секции, найдено ' + htmlKeys.length);
        const missing = htmlKeys.filter(function (k) { return RU[k] === undefined; });
        assert.deepStrictEqual(missing, [], 'нет в ru.js: ' + missing.join(', '));
    });

    check('image.profiles.* объявлены парно в en.js и ru.js', function () {
        const EN_SRC = fs.readFileSync(path.join(__dirname, '..', 'i18n', 'en.js'), 'utf8');
        const collect = function (src) {
            const re = /"(image\.profiles\.[A-Za-z0-9_.]+)"\s*:/g;
            const out = [];
            let m;
            while ((m = re.exec(src)) !== null) out.push(m[1]);
            return out.sort();
        };
        const ru = collect(RU_SRC);
        const en = collect(EN_SRC);
        assert.ok(ru.length >= 80, 'ожидалось >= 80 ключей image.profiles.*, найдено ' + ru.length);
        assert.deepStrictEqual(en, ru, 'наборы ключей en/ru расходятся');
    });

    // --- 10. разметка редактора ------------------------------------------
    check('разметка редактора: id уникальны, поля на месте, теги закрыты', function () {
        const html = IP._actions.editorHtmlForTest(IP.pure.profileToForm({
            name: 'flux-q4', family: 'flux',
            files: [
                { role: 'diffusion', repo: 'r', filename: 'f.gguf', localPath: '/models/f.gguf', sizeBytes: 1000 },
                { role: 'vae', repo: 'r', filename: 'ae.safetensors' }
            ],
            defaults: { steps: 4, cfgScale: 1, width: 1024, height: 1024, batchCount: 1, seed: -1 },
            runtime: { vaeTiling: true, seedMode: 'random', extraArgs: ['--foo'] }
        }, true), false);

        // Уникальность id: дубль ломает getElementById в браузере.
        const ids = Array.from(html.matchAll(/\bid="([A-Za-z0-9_-]+)"/g)).map(function (m) { return m[1]; });
        const dupes = ids.filter(function (id, i) { return ids.indexOf(id) !== i; });
        assert.deepStrictEqual(dupes, [], 'дубли id в разметке редактора: ' + dupes.join(', '));

        // Обязательные id.
        ['imgpEditorOverlay', 'imgpErrors', 'imgpName', 'imgpFamily', 'imgpDisabled', 'imgpVram',
            'imgpTimeout', 'imgpIdleUnload', 'imgpNotes', 'imgpPreset', 'imgpFileRows', 'imgpFileAdd',
            'imgpSteps', 'imgpCfg', 'imgpWidth', 'imgpHeight', 'imgpBatch', 'imgpSeed', 'imgpClipSkip',
            'imgpSampler', 'imgpScheduler', 'imgpNegative', 'imgpRuntimeBackend', 'imgpParamsBackend',
            'imgpMaxVram', 'imgpAutoFit', 'imgpSplitMode', 'imgpGpuLayers', 'imgpThreads',
            'imgpVaeTileSize', 'imgpSeedMode', 'imgpOffloadToCpu', 'imgpDiffusionFa', 'imgpVaeTiling',
            'imgpVaeConvDirect', 'imgpTaesd', 'imgpExtraArgs', 'imgpServerArgs', 'imgpCancel',
            'imgpSave', 'imgpSaveApply'].forEach(function (id) {
            assert.ok(ids.indexOf(id) !== -1, 'нет id ' + id + ' в разметке редактора');
        });

        // Значения из профиля реально попадают в поля (иначе форма «пустая»).
        assert.ok(/id="imgpName"[^>]*value="flux-q4"/.test(html), 'имя не подставлено');
        assert.ok(/id="imgpWidth"[^>]*value="1024"/.test(html), 'ширина не подставлена');
        assert.ok(/id="imgpOffloadToCpu"[^>]*data-imgp-field/.test(html), 'чекбокс offload не размечен');
        const widthTag = (html.match(/<input type="number"[^>]*id="imgpWidth"[^>]*>/) || [''])[0];
        assert.ok(/step="64"/.test(widthTag), 'у ширины нет шага 64: ' + widthTag);
        assert.ok(/step="64"/.test((html.match(/<input type="number"[^>]*id="imgpHeight"[^>]*>/) || [''])[0]), 'у высоты нет шага 64');
        assert.ok(html.indexOf('--foo') !== -1, 'extraArguments не попали в textarea');
        assert.ok(html.indexOf('/models/f.gguf') !== -1, 'localPath не показан оператору');
        // Панель предпросмотра argv существует, а её содержимое рисует
        // renderServerArgs из previewServerArgs (проверено в pure-тесте).
        assert.ok(html.indexOf('id="imgpServerArgs"') !== -1, 'нет контейнера предпросмотра argv');
        assert.ok(IP._actions.previewServerArgs(IP.pure.formToProfile(IP.pure.profileToForm({
            family: 'flux',
            files: [{ role: 'diffusion', repo: 'r', filename: 'f.gguf' }, { role: 'vae', repo: 'r', filename: 'ae.safetensors' }],
            defaults: { steps: 4, cfgScale: 1, width: 1024, height: 1024, batchCount: 1, seed: -1 },
            runtime: { seedMode: 'random' }
        }, true))).indexOf('--diffusion-model') !== -1, 'DiT-профиль без --diffusion-model');
        assert.ok(html.indexOf('undefined') === -1, 'в разметке есть undefined');

        // Баланс тегов: открывающие/закрывающие пары для контейнерных тегов.
        ['div', 'details', 'summary', 'select', 'textarea', 'label', 'small', 'pre', 'ul', 'li', 'h3', 'span', 'datalist'].forEach(function (tag) {
            const open = (html.match(new RegExp('<' + tag + '(?=[\\s>])', 'g')) || []).length;
            const close = (html.match(new RegExp('</' + tag + '>', 'g')) || []).length;
            assert.strictEqual(open, close, 'несбалансированные <' + tag + '>: ' + open + ' открыт / ' + close + ' закрыт');
        });

        // Никаких «сырых» значений, ломающих разметку.
        const injection = IP._actions.editorHtmlForTest(IP.pure.profileToForm({
            name: '"><img src=x onerror=alert(1)>', family: 'sd15',
            files: [{ role: 'diffusion', repo: 'r"><b>', filename: 'f.gguf' }],
            defaults: { steps: 1, cfgScale: 1, width: 512, height: 512, batchCount: 1, seed: -1 },
            runtime: {}
        }, true), true);
        assert.strictEqual(injection.indexOf('<img src=x'), -1, 'значение профиля не экранировано (XSS)');
        assert.ok(injection.indexOf('&lt;img src=x') !== -1, 'ожидалось HTML-экранирование');
    });

    console.log('\n' + (failures.length ? 'FAILED: ' + failures.length : 'OK: ' + passed + ' checks passed'));
    if (failures.length) {
        failures.forEach(function (f) { console.error(' - ' + f); });
        process.exit(1);
    }
})();
