// cppworker-reasoning-defaults.test.js — R106b (2026-10-12).
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «через webUI не дало во вкладке настройки бекенда на
// странице gguf models поменять параметры размышления с выключенного на
// включенный … сейчас просто список общих настроек который ещё и не применяется,
// хотя «Сохранить» отрабатывает».
//
// ПОЧЕМУ ЭТОТ ТЕСТ. Поле reasoning в профиле — *bool, то есть у него ТРИ
// состояния: «не задавать» (наследовать), «включено», «выключено». В WebUI его не
// было НИ В ОДНОЙ форме: «настройки по умолчанию для новых моделей» умели задавать
// окно, слоты, KV-cache и flash, но не размышления; в профиле конкретной модели
// поля тоже не было. Плюс сервер (до 0.7.67) не читал enableReasoning из
// defaultModelProfile, поэтому список показывал «off (env контейнера)» сразу после
// сохранения «включено» — со стороны оператора это выглядело как «галочка не
// применяется».
//
// ЧТО ПРОВЕРЯЕТСЯ:
//   1. в форме «настройки по умолчанию» есть выбор reasoning с ТРЕМЯ состояниями;
//   2. сохранение отправляет enableReasoning=true / false (а пустой выбор — не
//      отправляет вовсе, чтобы не перетирать слой);
//   3. список «что получат новые модели» показывает значение из профиля как
//      источник «настройки по умолчанию», даже если сервер (старая сборка) отдал
//      «env контейнера»;
//   4. в профиле конкретной модели (мастер) появился выбор reasoning, и он
//      доезжает до PUT профиля — это и есть «поменять настройки для уже
//      загруженной модели» (дальше кнопка «Применить (reload)»);
//   5. все i18n-ключи новой разметки есть в ru.js.
//
// Запуск: node webui/js/modules/cppworker-reasoning-defaults.test.js

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

const MODULE_PATH = path.join(__dirname, 'cppworker-params.js');
const MODULE_SRC = fs.readFileSync(MODULE_PATH, 'utf8');

// --- mocks: window / i18n / утилиты ---------------------------------------
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
global.setTimeout = setTimeout;
global.clearTimeout = clearTimeout;
// Модуль подписывается на появление секции настроек через MutationObserver —
// в node его нет; для теста достаточно пустой заглушки.
global.MutationObserver = function () {
    this.observe = function () {};
    this.disconnect = function () {};
    this.takeRecords = function () { return []; };
};

// Реальный ru.js: тест заодно ловит отсутствующие переводы.
require('../i18n/ru.js');
const RU = global.I18N_RU;
assert.ok(RU, 'window.I18N_RU должен быть загружен из ru.js');
global.I18N = {
    getLang: function () { return 'ru'; },
    t: function (key, vars) {
        let s = Object.prototype.hasOwnProperty.call(RU, key) ? RU[key] : key;
        if (vars && typeof vars === 'object') {
            Object.keys(vars).forEach(function (k) { s = s.replace('{' + k + '}', vars[k]); });
        }
        return s;
    },
};
global.Utils = {
    escapeHtml: function (s) { return String(s == null ? '' : s); },
    updateGpuLayersBadge: function () {},
};
const toasts = [];
global.showToast = function (kind, msg) { toasts.push({ kind: kind, msg: msg }); };

// --- mocks: DOM ------------------------------------------------------------
const elements = {};
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
        addEventListener: function (type, fn) {
            (this._listeners[type] = this._listeners[type] || []).push(fn);
        },
        removeEventListener: function () {},
        dispatch: function (type, ev) {
            (this._listeners[type] || []).forEach(function (fn) { fn(ev || { target: this }); });
        },
        getAttribute: function () { return null; },
        setAttribute: function () {},
        // Любой селектор возвращает стабильный элемент: для '#id' — тот же id
        // (обработчики кликов и чтение значений совпадают), для остального —
        // отдельный ключ. Этого достаточно: модуль работает со своим блоком.
        querySelector: function (sel) {
            if (sel && sel.length > 1 && sel[0] === '#') return getEl(sel.slice(1));
            return getEl('sel:' + sel);
        },
        querySelectorAll: function () { return []; },
        closest: function () { return null; },
        appendChild: function () {},
        removeChild: function () {},
        remove: function () {},
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
const calls = { upsert: null, save: null };
const LOAD_DEFAULTS = {
    defaultModelProfile: { contextLength: 65536, parallel: 2, enableReasoning: true },
    // Сервер СТАРОЙ сборки: effective/source про reasoning врут (только env).
    effective: { contextLength: 65536, parallel: 2, enableReasoning: false },
    source: { contextLength: 'defaultProfile', parallel: 'defaultProfile', enableReasoning: 'cppworker-env' },
    cppworker: { available: true, backendId: 'cppworker-gpu-bundled-agent', config: { defaultEnableReasoning: false } },
    profilesCount: 18,
};
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
        get: function () { return Promise.resolve(LOAD_DEFAULTS); },
        save: function (body) {
            calls.save = body;
            return Promise.resolve({ status: 'ok', profile: body });
        },
        catalog: function () { return Promise.resolve({ files: [] }); },
    },
    cppworkerActiveQueries: { get: function () { return Promise.resolve({}); } },
    cppworkerApplyProgress: { streamProgress: function () { return { close: function () {} }; }, pollStatus: function () { return Promise.resolve({}); } },
};

require('./cppworker-params.js');
const P = window.CppWorkerParams;
assert.ok(P, 'window.CppWorkerParams должен быть определён');

// --- 1/2/3. Настройки по умолчанию для новых моделей ----------------------
(async function run() {
    getEl('cppLoadDefaults');
    await P.loadAndRender();

    check('в форме «настройки по умолчанию» есть выбор reasoning (3 состояния)', function () {
        assert.ok(/id="cppDefReasoning"/.test(MODULE_SRC),
            'форма не содержит поля cppDefReasoning — включить размышления для новых моделей из WebUI нельзя');
        const srcSlice = MODULE_SRC.slice(MODULE_SRC.indexOf("id=\"cppDefReasoning\""), MODULE_SRC.indexOf("id=\"cppDefReasoning\"") + 1400);
        assert.ok(/value="on"/.test(srcSlice) && /value="off"/.test(srcSlice),
            'у поля reasoning должны быть состояния «включено» и «выключено»');
    });

    check('список показывает reasoning из профиля как «настройки по умолчанию»', function () {
        const html = getEl('cppLoadDefaults').innerHTML;
        assert.ok(/reasoning/i.test(html), 'в списке нет строки reasoning: ' + html.slice(0, 400));
        assert.ok(/включено/.test(html),
            'значение профиля (enableReasoning=true) не показано — список врёт про env контейнера');
        assert.ok(/настройки по умолчанию/.test(html),
            'источник должен быть «настройки по умолчанию», а не «env контейнера»');
    });

    // Открываем форму правки и сохраняем «выключено».
    getEl('cppDefaultsEditBtn').dispatch('click');
    getEl('cppDefReasoning').value = 'off';
    getEl('cppDefCtx').value = '65536';
    getEl('cppDefIgnore').checked = false;
    getEl('cppDefaultsSave').dispatch('click');
    await new Promise(function (r) { setTimeout(r, 0); });

    check('сохранение отправляет enableReasoning=false (явное выключение)', function () {
        assert.ok(calls.save, 'LOAD_DEFAULTS.save не вызван — «Сохранить» ничего не отправляет');
        assert.strictEqual(calls.save.enableReasoning, false,
            'явное «выключено» не ушло в API: ' + JSON.stringify(calls.save) +
            ' (сервер различает «не прислали» и «прислали false», иначе выключить нельзя)');
        assert.strictEqual(calls.save.contextLength, 65536, 'n_ctx не уехал');
    });

    // «включено» — тоже отправляем (это и есть исходная жалоба).
    calls.save = null;
    getEl('cppDefaultsEditBtn').dispatch('click');
    getEl('cppDefReasoning').value = 'on';
    getEl('cppDefCtx').value = '65536';
    getEl('cppDefIgnore').checked = false;
    getEl('cppDefaultsSave').dispatch('click');
    await new Promise(function (r) { setTimeout(r, 0); });
    check('сохранение отправляет enableReasoning=true (включение размышлений)', function () {
        assert.ok(calls.save, 'LOAD_DEFAULTS.save не вызван');
        assert.strictEqual(calls.save.enableReasoning, true,
            'включение размышлений не ушло в API: ' + JSON.stringify(calls.save));
    });

    // «не задавать» — ключ не отправляем.
    calls.save = null;
    getEl('cppDefaultsEditBtn').dispatch('click');
    getEl('cppDefReasoning').value = '';
    getEl('cppDefCtx').value = '65536';
    getEl('cppDefIgnore').checked = false;
    getEl('cppDefaultsSave').dispatch('click');
    await new Promise(function (r) { setTimeout(r, 0); });
    check('«не задавать» не отправляет enableReasoning (слой не перетирается)', function () {
        assert.ok(calls.save, 'LOAD_DEFAULTS.save не вызван');
        assert.ok(!Object.prototype.hasOwnProperty.call(calls.save, 'enableReasoning'),
            'пустой выбор не должен попадать в тело: ' + JSON.stringify(calls.save));
    });

    // --- 4. Профиль конкретной модели (мастер) ----------------------------
    check('в мастере профиля модели есть выбор reasoning', function () {
        assert.ok(/id="wizEnableReasoning"/.test(MODULE_SRC),
            'в мастере нет поля wizEnableReasoning — включить размышления конкретной модели нечем');
        assert.ok(/allowedReasoning/.test(MODULE_SRC), 'нет валидации состояния reasoning');
    });

    // Значения полей мастера, которые читает readWizardState.
    getEl('wizCtxSlider').value = '32768';
    getEl('wizBatchSize').value = '512';
    getEl('wizNumGpuLayers').value = '-1';
    getEl('wizFlashAttn').value = '';
    getEl('wizNUMA').value = '';
    getEl('wizUseMmap').value = '';
    getEl('wizParallel').value = '0';
    getEl('wizKVCacheType').value = '';
    getEl('wizNotes').value = '';
    getEl('wizStreamingTimeout').value = '0';
    getEl('wizStreamingIdleTimeout').value = '0';
    getEl('wizRequestTimeout').value = '0';
    getEl('wizFirstByteTimeout').value = '0';
    getEl('wizCtxAuto').checked = false;
    getEl('wizCtxMax').value = '0';
    getEl('wizPrimary').checked = false;
    getEl('wizModelName').value = 'gemma-4-E4B-it-Q4_K_M';

    P.openWizard('gemma-4-E4B-it-Q4_K_M', { contextLength: 65536, parallel: 1 });
    getEl('wizEnableReasoning').value = 'on';
    getEl('wizCtxSlider').value = '65536';
    getEl('wizSave').dispatch('click');
    await new Promise(function (r) { setTimeout(r, 0); });

    check('профиль модели сохраняется с enableReasoning=true', function () {
        assert.ok(calls.upsert, 'PROFILES.upsert не вызван — сохранение профиля не сработало');
        assert.strictEqual(calls.upsert.name, 'gemma-4-E4B-it-Q4_K_M');
        assert.strictEqual(calls.upsert.body.enableReasoning, true,
            'reasoning не уехал в профиль модели: ' + JSON.stringify(calls.upsert.body) +
            ' — именно так «поменять настройки уже загруженной модели» и должно работать');
    });

    // --- 5. i18n ---------------------------------------------------------
    check('i18n-ключи новой разметки есть в ru.js', function () {
        const keys = [
            'settings.load_defaults.reasoning',
            'settings.load_defaults.reasoning_inherit',
            'settings.load_defaults.reasoning_on',
            'settings.load_defaults.reasoning_off',
            'settings.load_defaults.reasoning_help',
            'settings.profiles.enable_reasoning',
            'settings.profiles.enable_reasoning_help',
        ];
        const missing = keys.filter(function (k) { return !Object.prototype.hasOwnProperty.call(RU, k); });
        assert.strictEqual(missing.length, 0, 'нет переводов: ' + missing.join(', '));
    });

    // --- Итог -------------------------------------------------------------
    console.log('');
    if (failures.length) {
        console.error('cppworker-reasoning-defaults: ' + failures.length + ' FAILED, ' + passed + ' passed');
        failures.forEach(function (f) { console.error('  - ' + f); });
        process.exit(1);
    }
    console.log('cppworker-reasoning-defaults: OK (' + passed + ' checks passed)');
})();
