// cppworker-params-r400.test.js — R400/R401 (2026-11-05).
//
// ЖИВЫЕ ЖАЛОБЫ ОПЕРАТОРА:
//   1. «настроив загруженную модель под больший контекст, балансер не ищет
//      доступную возможную, а встречает зашитый блок в ограничение 131072 по
//      контекстному окну»;
//   2. «флаг primary бесполезен и непонятно зачем нужен».
//
// ПРИЧИНА (1). В карточке профиля два поля: «окно» (contextLength) и
// «максимальный контекст» (contextLengthMax). Оператор поднял окно до 262144, а
// максимум остался 131072 — и сервер брал потолок ТОЛЬКО из максимума, то есть
// урезал окно, которое оператор сам же заявил. Теперь WebUI при сохранении
// поднимает максимум до окна (ctxMaxEffective), а сервер (R400) считает потолок
// как max(contextLengthMax, contextLength).
//
// ПРИЧИНА (2). Флаг primary (наше изобретение R83) запрещал рост окна и
// подменял клиентский num_ctx. Он отключён: галочки в форме нет, при сохранении
// primary=false, сервер флаг игнорирует.
//
// Run: node webui/js/modules/cppworker-params-r400.test.js

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

let passed = 0;
const failures = [];
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

const MODULE_PATH = path.join(__dirname, 'cppworker-params.js');
const MODULE_SRC = fs.readFileSync(MODULE_PATH, 'utf8');

// --- mocks: window / i18n / utils -----------------------------------------
global.window = global;
global.addEventListener = function () {};
global.removeEventListener = function () {};
global.confirm = function () { return false; };
const store = {};
global.localStorage = {
    getItem: function (k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem: function (k, v) { store[k] = String(v); },
    removeItem: function (k) { delete store[k]; },
};
global.MutationObserver = function () {
    this.observe = function () {};
    this.disconnect = function () {};
    this.takeRecords = function () { return []; };
};

require('../i18n/ru.js');
require('../i18n/en.js');
const RU = global.I18N_RU;
const EN = global.I18N_EN;
assert.ok(RU, 'window.I18N_RU должен быть загружен из ru.js');
assert.ok(EN, 'window.I18N_EN должен быть загружен из en.js');
global.I18N = {
    getLang: function () { return 'ru'; },
    t: function (key, fallback) {
        if (Object.prototype.hasOwnProperty.call(RU, key)) return RU[key];
        return fallback === undefined ? key : fallback;
    },
};
global.Utils = {
    escapeHtml: function (s) { return String(s == null ? '' : s); },
    updateGpuLayersBadge: function () {},
};
global.showToast = function () {};

// --- mocks: DOM ------------------------------------------------------------
const elements = {};
function makeElement(id) {
    const el = {
        id: id, style: {}, _html: '', textContent: '', value: '', checked: false,
        disabled: false, _listeners: {},
        addEventListener: function (type, fn) { (this._listeners[type] = this._listeners[type] || []).push(fn); },
        removeEventListener: function () {},
        dispatch: function (type, ev) { (this._listeners[type] || []).forEach(function (fn) { fn(ev || { target: this }); }); },
        getAttribute: function () { return null; },
        setAttribute: function () {},
        querySelector: function (sel) {
            if (sel && sel.length > 1 && sel[0] === '#') return getEl(sel.slice(1));
            return getEl('sel:' + sel);
        },
        querySelectorAll: function () { return []; },
        closest: function () { return null; },
        appendChild: function () {}, removeChild: function () {}, remove: function () {},
        focus: function () {},
        classList: { add: function () {}, remove: function () {}, contains: function () { return false; }, toggle: function () {} },
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
global.document = {
    getElementById: getEl,
    querySelector: function (sel) {
        if (sel && sel.length > 1 && sel[0] === '#') return getEl(sel.slice(1));
        return getEl('sel:' + sel);
    },
    querySelectorAll: function () { return []; },
    createElement: function (tag) { return makeElement('created-' + tag); },
    body: { appendChild: function () {}, removeChild: function () {} },
    addEventListener: function () {},
};

// --- mocks: API ------------------------------------------------------------
const calls = { upsert: null };
global.Api = {
    cppworkerModelProfiles: {
        list: function () { return Promise.resolve({ models: {} }); },
        get: function (name) { return Promise.resolve({ model: name, profile: {} }); },
        upsert: function (name, body) {
            calls.upsert = { name: name, body: body };
            return Promise.resolve({ profile: body });
        },
        remove: function () { return Promise.resolve({}); },
        apply: function () { return Promise.resolve({ status: 'ok' }); },
    },
    cppworkerLoadDefaults: {
        get: function () { return Promise.resolve({ defaultModelProfile: {}, effective: {}, source: {}, cppworker: {} }); },
        save: function (body) { return Promise.resolve({ status: 'ok', profile: body }); },
        catalog: function () { return Promise.resolve({ files: [] }); },
    },
    cppworkerActiveQueries: { get: function () { return Promise.resolve({}); } },
    cppworkerApplyProgress: { streamProgress: function () { return { close: function () {} }; }, pollStatus: function () { return Promise.resolve({}); } },
};

require('./cppworker-params.js');
const P = window.CppWorkerParams;
assert.ok(P, 'window.CppWorkerParams должен быть определён');

function fillWizardFields(windowCtx, maxCtx) {
    getEl('wizCtxSlider').value = String(windowCtx);
    getEl('wizCtxMax').value = String(maxCtx);
    getEl('wizCtxAuto').checked = true;
    getEl('wizBatchSize').value = '512';
    getEl('wizNumGpuLayers').value = '-2';
    getEl('wizFlashAttn').value = '';
    getEl('wizNUMA').value = '';
    getEl('wizUseMmap').value = '';
    getEl('wizParallel').value = '0';
    getEl('wizKVCacheType').value = '';
    getEl('wizEnableReasoning').value = '';
    getEl('wizNotes').value = '';
    getEl('wizModelName').value = 'Qwen3.8-27B-UD-Q4_K_M';
}

function tick() {
    return new Promise(function (r) { setTimeout(r, 0); });
}

(async function run() {
    await check('в форме больше нет галочки primary', function () {
        assert.ok(!/id="wizPrimary"/.test(MODULE_SRC),
            'в мастере профиля осталась галочка wizPrimary — флаг primary отключён (R401)');
        assert.ok(!/'settings\.profiles\.primary(_help)?'/.test(MODULE_SRC),
            'форма всё ещё читает settings.profiles.primary');
    });

    await check('R400: окно выше максимума — максимум поднимается до окна', async function () {
        calls.upsert = null;
        P.openWizard('Qwen3.8-27B-UD-Q4_K_M', {
            contextLength: 262144, contextLengthMax: 131072, contextLengthAuto: true,
        });
        fillWizardFields(262144, 131072);
        getEl('wizSave').dispatch('click');
        await tick();
        assert.ok(calls.upsert, 'PROFILES.upsert не вызван — сохранение профиля не сработало');
        assert.strictEqual(calls.upsert.body.contextLength, 262144,
            'окно не уехало в профиль: ' + JSON.stringify(calls.upsert.body));
        assert.strictEqual(calls.upsert.body.contextLengthMax, 262144,
            'максимум остался ниже окна (' + calls.upsert.body.contextLengthMax +
            ') — сервер урежет окно и клиент получит «модель поддерживает 131072»');
    });

    await check('R400: максимум выше окна сохраняется как задан (не затирается)', async function () {
        calls.upsert = null;
        P.openWizard('gemma-4-E4B-it-Q4_K_M', {
            contextLength: 32768, contextLengthMax: 131072, contextLengthAuto: true,
        });
        fillWizardFields(32768, 131072);
        getEl('wizSave').dispatch('click');
        await tick();
        assert.ok(calls.upsert, 'PROFILES.upsert не вызван');
        assert.strictEqual(calls.upsert.body.contextLengthMax, 131072,
            'максимум не должен меняться, если он выше окна: ' + JSON.stringify(calls.upsert.body));
    });

    await check('R401: сохранение профиля снимает устаревший primary', async function () {
        calls.upsert = null;
        P.openWizard('Qwen3.8-27B', { contextLength: 262144, contextLengthMax: 262144, primary: true });
        fillWizardFields(262144, 262144);
        getEl('wizSave').dispatch('click');
        await tick();
        assert.ok(calls.upsert, 'PROFILES.upsert не вызван');
        assert.strictEqual(calls.upsert.body.primary, false,
            'primary=true остался в теле профиля, хотя флаг отключён: ' + JSON.stringify(calls.upsert.body));
    });

    await check('i18n: подсказка о поднятии максимума есть в ru.js и en.js', function () {
        assert.ok(Object.prototype.hasOwnProperty.call(RU, 'settings.profiles.ctx_max_raised'),
            'нет перевода settings.profiles.ctx_max_raised в ru.js');
        assert.ok(Object.prototype.hasOwnProperty.call(EN, 'settings.profiles.ctx_max_raised'),
            'нет перевода settings.profiles.ctx_max_raised в en.js');
        assert.ok(!Object.prototype.hasOwnProperty.call(RU, 'settings.profiles.primary'),
            'мёртвый перевод settings.profiles.primary остался в ru.js');
        assert.ok(!Object.prototype.hasOwnProperty.call(EN, 'settings.profiles.primary'),
            'мёртвый перевод settings.profiles.primary остался в en.js');
    });

    console.log('');
    if (failures.length) {
        console.error('cppworker-params-r400: ' + failures.length + ' FAILED, ' + passed + ' passed');
        failures.forEach(function (f) { console.error('  - ' + f); });
        process.exit(1);
    }
    console.log('cppworker-params-r400: OK (' + passed + ' checks passed)');
})();
