// webui/tests/gguf-async-blocks-cache-r691.test.js — R691 (2026-11-23): пересборка
// правой панели GGUF не возвращает асинхронные блоки в заглушку «Загрузка…».
//
// ЖИВАЯ ЖАЛОБА (из отчёта R689, подтверждена замером на r690): при РЕАЛЬНОМ
// изменении данных правая панель пересобирается (R689: только когда данные
// изменились), и блоки, которые рисуются ПОСЛЕ панели по ответам API, снова
// становятся заглушкой до повторного клика по вкладке/модели:
//   * «что получат новые модели» — [data-role="cpp-load-defaults"];
//   * «модели в папке»           — [data-role="cpp-model-catalog"];
//   * список профилей моделей    — #cppProfilesList;
//   * «параметры запуска модели» — .gguf-mp-editor[data-model] (значения профиля
//     и «предел окна» приходят отдельным запросом).
//
// ЧТО ПРОВЕРЯЕТСЯ (поведенчески, на НАСТОЯЩИХ модулях из WEBUI_SRC_ROOT):
//   1. пересборка панели → блоки показывают последнее содержимое, а не заглушку;
//   2. фоновое обновление приходит (профили/дефолты/каталог перезапрошены) и НЕ
//      перерисовывает блок, если ответ тот же (нет моргания);
//   3. заглушка остаётся только там, где данных в этом контексте ещё не было;
//   4. смена модели → кэш не применяется (чужие значения не подставляются);
//   5. инвалидация: после сохранения (noteSaved) и явного «Обновить» кэш не
//      считается свежим, но содержимое не стирается; авто-проход не инвалидирует;
//   6. смена страницы стирает кэш; устаревший ответ не «перебивает» свежий;
//   7. статическая обвязка: порядок скриптов, вызов restore после пересборки,
//      регистрация блоков, инвалидация в noteSaved/Reload.
//
// ТЕСТ НЕ-ВАКУУМНЫЙ. Он запускается на настоящих модулях каталога WEBUI_SRC_ROOT
// (по умолчанию — текущий webui/) и на коде до R691 КРАСНЕЕТ: модуля
// js/modules/gguf-async-cache.js там нет, restore после пересборки не зовётся, и
// блоки после подмены DOM остаются заглушкой:
//
//   git archive 1b2287fd webui | tar -x -C <tmp>
//   WEBUI_SRC_ROOT=<tmp>/webui node webui/tests/gguf-async-blocks-cache-r691.test.js
//
// Запуск: node webui/tests/gguf-async-blocks-cache-r691.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const SRC = process.env.WEBUI_SRC_ROOT
    ? path.resolve(process.env.WEBUI_SRC_ROOT)
    : path.join(__dirname, '..');
const read = (rel) => fs.readFileSync(path.join(SRC, rel), 'utf8');

// --- фейковый DOM ------------------------------------------------------------
//
// Панель/содержимое — как в браузере: innerHTML панели ПЕРЕСОЗДАЁТ блоки
// (именно это и уничтожало асинхронно дорисованное содержимое). Блоки
// вынимаются из разметки по id/data-role, редакторы параметров модели — узлы,
// которые тест создаёт на каждую отрисовку (так же, как их создаёт рендер).

function stripTags(html) {
    return String(html === null || html === undefined ? '' : html)
        .replace(/<[^>]*>/g, ' ')
        .replace(/\s+/g, ' ')
        .trim();
}

// Сколько обработчиков навешано на узлы всего: по этому счётчику видно, что
// восстановленный из кэша блок нарисован ШТАТНЫМ рендерером (кнопки «Изменить» /
// «Настроить» снова живые), а не вставлен готовым HTML.
let listenerBindings = 0;

function makeLeaf(attrs, tagName) {
    const el = {
        tagName: String(tagName || 'DIV').toUpperCase(),
        id: '',
        style: {},
        dataset: {},
        className: '',
        _attrs: Object.assign({}, attrs || {}),
        _html: '',
        _writes: 0,
        _listeners: 0,
        parentNode: null,
        children: [],
        classList: { add() {}, remove() {}, contains() { return false; }, toggle() {} }
    };
    el.id = el._attrs.id || '';
    el.getAttribute = function (n) {
        return Object.prototype.hasOwnProperty.call(el._attrs, n) ? el._attrs[n] : null;
    };
    el.setAttribute = function (n, v) {
        el._attrs[n] = String(v);
        if (n === 'id') { el.id = String(v); }
    };
    el.removeAttribute = function (n) { delete el._attrs[n]; };
    el.addEventListener = function () { listenerBindings++; el._listeners++; };
    el.removeEventListener = function () {};
    el.focus = function () {};
    el.contains = function (x) { return x === el; };
    el.appendChild = function (c) { if (c) { c.parentNode = el; el.children.push(c); } return c; };
    el.insertBefore = function (c) { if (c) { c.parentNode = el; el.children.push(c); } return c; };
    el.querySelector = function (sel) {
        const list = el.querySelectorAll(sel);
        return list && list.length ? list[0] : null;
    };
    el.querySelectorAll = function () { return []; };
    Object.defineProperty(el, 'innerHTML', {
        configurable: true,
        get: function () { return el._html; },
        set: function (v) { el._writes++; el._html = String(v); el.children = []; }
    });
    Object.defineProperty(el, 'textContent', {
        configurable: true,
        get: function () { return stripTags(el._html); },
        set: function (v) { el._html = String(v); }
    });
    Object.defineProperty(el, 'innerText', {
        configurable: true,
        get: function () { return stripTags(el._html); }
    });
    return el;
}

/** queryFromHtml — минимальный «querySelector» по разметке блока (id, data-action). */
function queryFromHtml(html, selector) {
    const out = [];
    String(selector || '').split(',').map((s) => s.trim()).filter(Boolean).forEach(function (sel) {
        const byId = /^#([\w-]+)$/.exec(sel);
        if (byId) {
            if (new RegExp('id="' + byId[1] + '"').test(html)) { out.push(makeLeaf({ id: byId[1] }, 'BUTTON')); }
            return;
        }
        const byAction = /^\[data-action(?:="([^"]+)")?\]$/.exec(sel);
        if (byAction) {
            const re = byAction[1]
                ? new RegExp('data-action="' + byAction[1] + '"', 'g')
                : /data-action="/g;
            const n = (String(html).match(re) || []).length;
            for (let i = 0; i < n; i++) { out.push(makeLeaf({ 'data-action': byAction[1] || '' }, 'BUTTON')); }
        }
    });
    return out;
}

function makeBlock(id, attrs) {
    const el = makeLeaf(attrs, 'DIV');
    if (id) { el.setAttribute('id', id); }
    el.querySelectorAll = function (sel) { return queryFromHtml(el._html, sel); };
    return el;
}

/** elementAt — вынуть элемент (с вложенным содержимым) из разметки по атрибуту. */
function elementAt(html, attrName, attrValue) {
    // Атрибут ищем ТОЧНО: «id» без значения совпал бы с первым попавшимся
    // элементом с любым id, а data-role без значения — с первым data-role.
    const attrPart = (attrValue === undefined || attrValue === null)
        ? '\\b' + attrName + '(?:="[^"]*")?'
        : '\\b' + attrName + '="' + String(attrValue).replace(/"/g, '\\"') + '"';
    const re = new RegExp('<' + '([a-zA-Z0-9]+)([^>]*' + attrPart + '[^>]*)>');
    const m = re.exec(html);
    if (!m) { return null; }
    const tag = m[1].toLowerCase();
    const attrs = {};
    const attrRe = /([\w:-]+)(?:="([^"]*)")?/g;
    let a;
    while ((a = attrRe.exec(m[2])) !== null) { attrs[a[1]] = a[2] === undefined ? '' : a[2]; }
    const start = m.index + m[0].length;
    let depth = 1;
    let end = html.length;
    const tagRe = new RegExp('<' + '(/?)' + tag + '\\b[^>]*>', 'gi');
    tagRe.lastIndex = start;
    let t;
    while ((t = tagRe.exec(html)) !== null) {
        if (t[1] === '/') {
            depth--;
            if (depth === 0) { end = t.index; break; }
        } else if (!/\/>$/.test(t[0])) {
            depth++;
        }
    }
    return { tag: tag, attrs: attrs, inner: html.slice(start, end) };
}

const BLOCK_SELECTORS = [
    { id: 'cppProfilesList', html: 'id="cppProfilesList"' },
    { id: 'cppLoadDefaults', role: 'cpp-load-defaults' },
    { id: 'cppModelCatalog', role: 'cpp-model-catalog' }
];

function makePane(pageEl) {
    const pane = makeLeaf({ id: 'ggufDetailContent' }, 'DIV');
    pane.parentNode = null;
    pane._blocks = [];
    pane._editors = [];
    Object.defineProperty(pane, 'innerHTML', {
        configurable: true,
        get: function () { return pane._html; },
        set: function (v) {
            pane._writes++;
            pane._html = String(v);
            pane._blocks = [];
            BLOCK_SELECTORS.forEach(function (spec) {
                const found = spec.role
                    ? elementAt(pane._html, 'data-role', spec.role)
                    : elementAt(pane._html, 'id', spec.id);
                if (!found) { return; }
                const block = makeBlock('');
                Object.keys(found.attrs).forEach(function (k) { block.setAttribute(k, found.attrs[k]); });
                block._html = found.inner;
                pane._blocks.push(block);
            });
        }
    });
    pane.querySelectorAll = function (selector) {
        const out = [];
        String(selector || '').split(',').map((s) => s.trim()).filter(Boolean).forEach(function (sel) {
            const byId = /^#([\w-]+)$/.exec(sel);
            if (byId) {
                pane._blocks.forEach(function (b) { if (b.id === byId[1]) { out.push(b); } });
                return;
            }
            const byRole = /^\[data-role="([^"]+)"\]$/.exec(sel);
            if (byRole) {
                pane._blocks.forEach(function (b) { if (b.getAttribute('data-role') === byRole[1]) { out.push(b); } });
                return;
            }
            if (sel === '.gguf-mp-editor[data-model]' || sel === '.gguf-mp-editor[data-model]:not([data-hydrated="1"])') {
                const onlyFresh = sel.indexOf(':not(') !== -1;
                (pane._editors || []).forEach(function (e) {
                    if (onlyFresh && e.getAttribute('data-hydrated') === '1') { return; }
                    out.push(e);
                });
                return;
            }
            if (sel === 'input[id], select[id], textarea[id]' || sel === 'details') { return; }
        });
        return out;
    };
    pane.contains = function (x) { return x === pane; };
    Object.defineProperty(pane, 'textContent', { configurable: true, get: function () { return stripTags(pane._html); } });
    return pane;
}

function makePanel(pane, pageEl) {
    const panel = makeLeaf({ id: 'ggufDetailPanel' }, 'DIV');
    panel.parentNode = pageEl || null;
    panel.querySelectorAll = function (sel) { return pane.querySelectorAll(sel); };
    panel.querySelector = function (sel) {
        const l = pane.querySelectorAll(sel);
        return l.length ? l[0] : null;
    };
    Object.defineProperty(panel, 'innerHTML', {
        configurable: true,
        get: function () { return panel._html; },
        set: function (v) {
            panel._writes++;
            panel._html = String(v);
            const m = /<div id="ggufDetailContent"[^>]*>([\s\S]*)<\/div>\s*$/.exec(panel._html);
            pane.innerHTML = m ? m[1] : panel._html;
        }
    });
    panel.contains = function (x) { return x === panel || pane.contains(x); };
    return panel;
}

/**
 * makeEditor — узел «параметры запуска модели» с ячейками профиля и полями.
 *
 * Ровно то, что читает/пишет настоящий fillModelParamEditor: ячейка
 * [data-profile-field], поле .gguf-mp-field[data-field], «предел окна»
 * .gguf-mp-limit (создаётся самим модулем через createElement/appendChild).
 */
function makeEditor(model) {
    const editor = makeLeaf({ 'data-model': model }, 'DIV');
    editor._model = model;
    editor._cells = {};
    editor._fields = {};
    editor._limit = null;
    editor._hydrated = false;
    editor.getAttribute = function (n) {
        if (n === 'data-model') { return model; }
        if (n === 'data-hydrated') { return editor._hydrated ? '1' : null; }
        return null;
    };
    editor.setAttribute = function (n, v) {
        if (n === 'data-hydrated') { editor._hydrated = String(v) === '1'; return; }
    };
    editor.removeAttribute = function (n) { if (n === 'data-hydrated') { editor._hydrated = false; } };
    editor.querySelector = function (sel) {
        let m = /^\[data-profile-field="([^"]+)"\]$/.exec(sel);
        if (m) {
            if (!editor._cells[m[1]]) { editor._cells[m[1]] = makeLeaf({}, 'TD'); }
            return editor._cells[m[1]];
        }
        m = /^\.gguf-mp-field\[data-field="([^"]+)"\]$/.exec(sel);
        if (m) {
            if (!editor._fields[m[1]]) {
                const f = makeLeaf({ 'data-field': m[1] }, 'INPUT');
                f.type = 'text';
                f.value = '';
                f.checked = false;
                f.placeholder = '';
                editor._fields[m[1]] = f;
            }
            return editor._fields[m[1]];
        }
        if (sel === '.gguf-mp-limit') { return editor._limit; }
        if (sel === '.gguf-mp-actions') { return null; }
        return null;
    };
    editor.querySelectorAll = function () { return []; };
    editor.appendChild = function (c) {
        if (c && String(c.className || '').indexOf('gguf-mp-limit') !== -1) { editor._limit = c; }
        return c;
    };
    editor.insertBefore = function (c) { return editor.appendChild(c); };
    editor.contains = function (x) { return x === editor; };
    return editor;
}

// --- окружение ---------------------------------------------------------------

const pageEl = { id: 'gguf-page', tagName: 'DIV', parentNode: null, scrollTop: 0, scrollLeft: 0 };
const pane = makePane(pageEl);
const panel = makePanel(pane, pageEl);

const created = [];
global.window = global;
global.document = {
    readyState: 'complete',
    activeElement: null,
    scrollingElement: pageEl,
    createElement: function (tag) {
        const el = makeLeaf({}, tag);
        el.querySelectorAll = function (sel) { return queryFromHtml(el._html, sel); };
        created.push(el);
        return el;
    },
    getElementById: function (id) {
        if (id === 'ggufDetailContent') { return pane; }
        if (id === 'ggufDetailPanel') { return panel; }
        if (id === 'gguf-page') { return pageEl; }
        const found = pane.querySelectorAll('#' + id);
        return found.length ? found[0] : null;
    },
    querySelector: function (sel) {
        const list = document.querySelectorAll(sel);
        return list.length ? list[0] : null;
    },
    querySelectorAll: function (sel) {
        const s = String(sel || '').trim();
        if (s === '.page.active') { return []; }
        // Остальные селекторы ищутся в содержимом панели: так же, как в браузере
        // document находит блоки #cppProfilesList / [data-role=…] внутри панели.
        return pane.querySelectorAll(s);
    },
    addEventListener: function () {},
    removeEventListener: function () {},
    visibilityState: 'visible'
};
global.App = { state: { currentPage: 'gguf' } };
global.I18N = {
    getLang: function () { return 'ru'; },
    t: function (k, fallback) { return fallback === undefined ? k : fallback; }
};
global.Utils = {
    escapeHtml: function (s) {
        return String(s === null || s === undefined ? '' : s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    },
    updateGpuLayersBadge: function () {}
};
global.showToast = function () {};
global.GgufModule = {
    state: {
        selectedBackendId: 'b1',
        detailPane: 'settings',
        registeredBackends: [{ id: 'b1', status: 'healthy' }],
        localModels: [],
        loadedModels: [],
        runtimeModels: {},
        diskInventory: {},
        modelProfiles: {},
        modelEffectiveWindow: {},
        nctxCeiling: null
    },
    _: function (key, fallback) { return fallback === undefined ? key : fallback; },
    showToast: function () {}
};

// Настоящие модули в том порядке, в котором их подключает index.html.
require(path.join(SRC, 'js', 'modules', 'refresh-guard.js'));
require(path.join(SRC, 'js', 'modules', 'form-dirty.js'));
const SafeForm = require(path.join(SRC, 'js', 'modules', 'safe-form.js'));
const cacheFile = path.join(SRC, 'js', 'modules', 'gguf-async-cache.js');
const cacheModuleExists = fs.existsSync(cacheFile);
const Cache = cacheModuleExists ? require(cacheFile) : null;
if (Cache && Cache._reset) { Cache._reset(); }
const guardFile = path.join(SRC, 'js', 'modules', 'gguf-detail-form-state.js');
const formState = fs.existsSync(guardFile) ? require(guardFile) : null;
require(path.join(SRC, 'js', 'modules', 'gguf-renderer-refresh.js'));

// --- моки API ----------------------------------------------------------------

const calls = { profiles: 0, defaults: 0, catalog: 0 };
let PROFILES_RESPONSE = {
    models: {
        'Qwen3.8-27B-UD-Q4_K_M': { contextLength: 32768, batchSize: 512, numGpuLayers: -1 },
        'gemma-4-E4B-it-Q4_K_M': { contextLength: 65536 }
    }
};
const DEFAULTS_RESPONSE = {
    defaultModelProfile: { contextLength: 65536, parallel: 2 },
    effective: { contextLength: 65536, parallel: 2, enableReasoning: true },
    source: { contextLength: 'defaultProfile', parallel: 'defaultProfile' },
    cppworker: { available: true, backendId: 'cppworker-gpu-bundled-agent' },
    profilesCount: 2
};
const CATALOG_RESPONSE = {
    files: [
        { name: 'Qwen3.8-27B-UD-Q4_K_M.gguf', sizeBytes: 16 * 1024 * 1024 * 1024, quantization: 'Q4_K_M', hasProfile: true, loaded: false, effective: { contextLength: 32768, parallel: 1, enableReasoning: true }, source: { contextLength: 'defaultProfile', parallel: 'defaultProfile' } },
        { name: 'gemma-4-E4B-it-Q4_K_M.gguf', sizeBytes: 4 * 1024 * 1024 * 1024, quantization: 'Q4_K_M', hasProfile: true, loaded: false, effective: { contextLength: 65536, parallel: 1, enableReasoning: true }, source: { contextLength: 'defaultProfile', parallel: 'defaultProfile' } }
    ]
};
global.Api = {
    cppworkerModelProfiles: {
        list: function () {
            calls.profiles++;
            return Promise.resolve({
                models: PROFILES_RESPONSE.models,
                effectiveContext: {
                    'Qwen3.8-27B-UD-Q4_K_M': { effectiveContextLength: 32768, limitReason: 'окно задано оператором' }
                },
                nctxCeiling: { envCeiling: 131072, reason: 'потолок авто-роста' }
            });
        },
        get: function (name) { return Promise.resolve({ model: name, profile: {} }); },
        upsert: function (name, body) { return Promise.resolve({ profile: body }); },
        remove: function () { return Promise.resolve({}); },
        apply: function () { return Promise.resolve({ status: 'ok' }); }
    },
    cppworkerLoadDefaults: {
        get: function () { calls.defaults++; return Promise.resolve(DEFAULTS_RESPONSE); },
        save: function (body) { return Promise.resolve({ status: 'ok', profile: body }); },
        catalog: function () { calls.catalog++; return Promise.resolve(CATALOG_RESPONSE); }
    }
};

require(path.join(SRC, 'js', 'modules', 'cppworker-params.js'));
require(path.join(SRC, 'js', 'modules', 'gguf-detail-params.js'));

const M = global.GgufModule;
const CppWorkerParams = window.CppWorkerParams;

// Фабрика параметров модели — ровно так её создаёт gguf-detail-settings.js;
// при создании она регистрирует блок «параметры модели» в кэше.
const SPECS = [
    { field: 'contextLength', label: 'gguf.mp_ctx', type: 'int', min: 256, max: 262144, defaultKey: 'contextLength' },
    { field: 'parallel', label: 'gguf.mp_parallel', type: 'int', min: 0, max: 8, defaultKey: 'parallel' }
];
const Params = window.GgufDetailParams({
    M: M,
    state: M.state,
    _: function (key, fallback) { return fallback === undefined ? key : fallback; },
    Utils: global.Utils,
    PARAM_SOURCE_LABELS: {},
    MODEL_PARAM_SPECS: SPECS,
    paramFact: function () { return null; },
    collapseSections: function (h) { return h; }
});
M.fillModelParamEditor = Params.fillModelParamEditor;
M.hydrateModelParamEditors = Params.hydrateModelParamEditors;

// --- разметка панели ---------------------------------------------------------

const LOADING = '<div class="loading">Загрузка…</div>';
let server = { version: 1, pane: 'settings' };

function renderPane() {
    if (M.state.detailPane === 'models') {
        // Вкладка «Модели»: карточки с редакторами параметров (значения — из API).
        pane._editors = [
            makeEditor('Qwen3.8-27B-UD-Q4_K_M'),
            makeEditor('gemma-4-E4B-it-Q4_K_M')
        ];
        return '<div class="gguf-pane-models">' +
            '<div class="gguf-local-card"><details class="gguf-mp-acc" data-model-acc="Qwen3.8-27B-UD-Q4_K_M">' +
                '<summary class="gguf-mp-summary">Параметры запуска этой модели</summary></details></div>' +
            '<div class="gguf-local-card"><details class="gguf-mp-acc" data-model-acc="gemma-4-E4B-it-Q4_K_M">' +
                '<summary class="gguf-mp-summary">Параметры запуска этой модели</summary></details></div>' +
        '</div>';
    }
    pane._editors = [];
    return '<div class="gguf-settings-form">' +
        '<div class="settings-subsection">' +
            '<div id="ggufBackendOptionsContainer"><input id="ggufOptCtxSize" type="number" value="' + (server.version === 1 ? 65536 : 32768) + '"><span>v' + server.version + '</span></div>' +
        '</div>' +
        '<div class="settings-subsection">' +
            '<div id="cppProfilesList"><div class="cpp-profiles-empty"></div></div>' +
            '<h4>Что получат НОВЫЕ модели</h4>' +
            '<div data-role="cpp-load-defaults">' + LOADING + '</div>' +
            '<h4>Модели в папке</h4>' +
            '<div data-role="cpp-model-catalog">' + LOADING + '</div>' +
        '</div>' +
    '</div>';
}

M.currentBackend = function () { return { id: 'b1', url: 'http://b1' }; };
M.renderDetailPane = function () { return renderPane(); };
M.renderDetailPanel = function () {
    // Статус бэкенда — в шапке, как в renderDetailHeader: его изменение и есть
    // «реальное изменение данных», которое пересобирает панель.
    const backend = (M.state.registeredBackends || []).find(function (b) { return b.id === M.state.selectedBackendId; }) || {};
    return '<div class="gguf-detail-header">b1 · ' + String(backend.status || '-') + '</div>' +
        '<div class="gguf-detail-tabs">Табы</div>' +
        '<div id="ggufDetailContent" class="gguf-detail-content">' + renderPane() + '</div>';
};

// --- каркас проверок ---------------------------------------------------------

const checks = [];
let passed = 0;
const failures = [];
function check(name, fn) { checks.push([name, fn]); }
function flush() {
    return new Promise(function (resolve) {
        setTimeout(function () {
            setTimeout(resolve, 5);
        }, 0);
    });
}
function blockText(id) {
    const found = pane.querySelectorAll('#' + id);
    if (found.length) { return found.length ? found[0].textContent : ''; }
    const role = id === 'cppLoadDefaults' ? 'cpp-load-defaults' : (id === 'cppModelCatalog' ? 'cpp-model-catalog' : '');
    const byRole = role ? pane.querySelectorAll('[data-role="' + role + '"]') : [];
    return byRole.length ? byRole[0].textContent : '';
}
function blockNode(id) {
    const direct = pane.querySelectorAll('#' + id);
    if (direct.length) { return direct[0]; }
    const role = id === 'cppLoadDefaults' ? 'cpp-load-defaults' : (id === 'cppModelCatalog' ? 'cpp-model-catalog' : '');
    const byRole = role ? pane.querySelectorAll('[data-role="' + role + '"]') : [];
    return byRole.length ? byRole[0] : null;
}
function isPlaceholder(text) {
    const t = String(text || '');
    return /Загрузка|Loading/i.test(t) && t.trim().length < 80;
}
function hasCache() { return !!(Cache && typeof Cache.restore === 'function'); }
function requireCache() {
    assert.ok(cacheModuleExists, 'нет модуля js/modules/gguf-async-cache.js — кэш асинхронных блоков не существует');
    assert.ok(hasCache(), 'window.GgufAsyncCache.restore не определён');
}

// --- (1) первая отрисовка блоков (как при первом открытии вкладки) -----------

check('R691(а): первая пересборка панели заполняет блоки по ответам API', async function () {
    requireCache();
    // Как в жизни: клик по табу «Настройки» → refreshDetailPanel → пересборка →
    // блоки в заглушке → restore ставит их фоновое обновление в очередь.
    M.refreshDetailPanel();
    assert.ok(isPlaceholder(blockText('cppLoadDefaults')),
        'предпосылка: до первого ответа блок «что получат новые модели» — заглушка');
    await flush();
    assert.ok(!isPlaceholder(blockText('cppLoadDefaults')), 'блок «что получат новые модели» не наполнился');
    assert.ok(!isPlaceholder(blockText('cppModelCatalog')), 'блок «модели в папке» не наполнился');
    assert.ok(/n_ctx/.test(blockText('cppLoadDefaults')), 'в блоке дефолтов нет n_ctx: ' + blockText('cppLoadDefaults').slice(0, 120));
    assert.ok(/Qwen3\.8-27B-UD-Q4_K_M/.test(blockText('cppModelCatalog')),
        'в каталоге нет имени модели: ' + blockText('cppModelCatalog').slice(0, 160));
    assert.ok(/Qwen3\.8-27B-UD-Q4_K_M/.test(blockText('cppProfilesList')),
        'в списке профилей нет имени модели: ' + blockText('cppProfilesList').slice(0, 160));
    assert.ok(calls.profiles >= 1 && calls.defaults >= 1 && calls.catalog >= 1,
        'блоки не запросили данные: ' + JSON.stringify(calls));
});

// --- (2) РЕАЛЬНОЕ изменение данных → пересборка → не заглушка ----------------

check('R691(б): пересборка при изменившихся данных показывает содержимое, а не заглушку', async function () {
    requireCache();
    const before = {
        defaults: blockText('cppLoadDefaults'),
        catalog: blockText('cppModelCatalog'),
        profiles: blockText('cppProfilesList')
    };
    const listenersBefore = listenerBindings;
    server.version = 2; // реальное изменение: в форме другое значение
    const rebuilt = M.refreshDetailPane();
    assert.strictEqual(rebuilt, true, 'предпосылка: изменившиеся данные обязаны пересобрать панель');
    assert.ok(listenerBindings > listenersBefore,
        'восстановленный блок нарисован без штатного рендерера: кнопки внутри остались без обработчиков');
    // САМОЕ ВАЖНОЕ: сразу после возврата из пересборки — ни одной заглушки.
    assert.ok(!isPlaceholder(blockText('cppLoadDefaults')),
        'после пересборки «что получат новые модели» — заглушка вместо содержимого');
    assert.ok(!isPlaceholder(blockText('cppModelCatalog')),
        'после пересборки «модели в папке» — заглушка вместо содержимого');
    assert.ok(/n_ctx/.test(blockText('cppLoadDefaults')),
        'содержимое блока дефолтов не восстановлено из кэша');
    assert.ok(/Qwen3\.8-27B-UD-Q4_K_M/.test(blockText('cppModelCatalog')),
        'содержимое каталога не восстановлено из кэша');
    assert.ok(/Qwen3\.8-27B-UD-Q4_K_M/.test(blockText('cppProfilesList')),
        'содержимое списка профилей не восстановлено из кэша');
    assert.ok(blockText('cppProfilesList').length >= before.profiles.length - 2,
        'список профилей стал короче: ' + before.profiles.length + ' → ' + blockText('cppProfilesList').length);
});

// --- (3) фоновое обновление приходит и не моргает ---------------------------

check('R691(в): фоновое обновление приходит, а равный ответ не перерисовывает блок', async function () {
    requireCache();
    const callsBefore = { profiles: calls.profiles, defaults: calls.defaults, catalog: calls.catalog };
    const writes = {
        defaults: blockNode('cppLoadDefaults')._writes,
        catalog: blockNode('cppModelCatalog')._writes,
        profiles: blockNode('cppProfilesList')._writes
    };
    await flush();
    assert.ok(calls.profiles > callsBefore.profiles, 'фоновое обновление не запросило профили');
    assert.ok(calls.defaults > callsBefore.defaults, 'фоновое обновление не запросило дефолты');
    assert.ok(calls.catalog > callsBefore.catalog, 'фоновое обновление не запросило каталог');
    // Ответ тот же → разметка та же → DOM блока не подменяется (нет моргания).
    assert.strictEqual(blockNode('cppLoadDefaults')._writes, writes.defaults,
        'блок дефолтов перерисован, хотя ответ не изменился');
    assert.strictEqual(blockNode('cppModelCatalog')._writes, writes.catalog,
        'блок каталога перерисован, хотя ответ не изменился');
    assert.strictEqual(blockNode('cppProfilesList')._writes, writes.profiles,
        'список профилей перерисован, хотя ответ не изменился');
});

// --- (4) заглушка — только там, где данных не было --------------------------

check('R691(г): без кэша блок остаётся заглушкой, но данные запрашиваются', async function () {
    requireCache();
    // Сильное забывание (как смена страницы): данных в этом контексте нет.
    Cache.invalidate('page', { hard: true });
    const callsBefore = { profiles: calls.profiles, defaults: calls.defaults, catalog: calls.catalog };
    server.version = 3;
    const rebuilt = M.refreshDetailPane();
    assert.strictEqual(rebuilt, true, 'предпосылка: данные изменились');
    assert.ok(isPlaceholder(blockText('cppLoadDefaults')),
        'без данных блок дефолтов обязан показывать заглушку (а не пустоту/старое)');
    await flush();
    assert.ok(!isPlaceholder(blockText('cppLoadDefaults')), 'заглушка не сменилась данными после ответа');
    assert.ok(calls.defaults > callsBefore.defaults && calls.catalog > callsBefore.catalog,
        'без кэша блоки не перезапрошены: ' + JSON.stringify(calls));
});

// --- (5) смена модели: чужой кэш не применяется -----------------------------

check('R691(д): смена модели — значения другой модели из кэша не подставляются', async function () {
    requireCache();
    // Вкладка «Модели»: раскрываем карточки — гидратация профилей.
    M.state.detailPane = 'models';
    M.refreshDetailPanel(); // первая отрисовка панели → restore ставит гидратацию
    await flush();
    let editorA = pane._editors.find((e) => e._model === 'Qwen3.8-27B-UD-Q4_K_M');
    assert.ok(editorA, 'редактор модели Qwen3.8-27B-UD-Q4_K_M не создан рендером');
    assert.ok(/32768/.test(editorA._cells.contextLength ? editorA._cells.contextLength.textContent : ''),
        'гидратация не подставила значение профиля (32768): ' +
        (editorA._cells.contextLength ? editorA._cells.contextLength.textContent : 'нет ячейки'));
    assert.ok(editorA._limit && /предел/i.test(editorA._limit.textContent || ''),
        '«предел окна» не нарисован после гидратации');

    // Пересборка панели: рендер создаёт НОВЫЕ узлы редакторов.
    M.state.registeredBackends[0].status = 'offline'; // реальное изменение данных
    const rebuilt = M.refreshDetailPanel();
    assert.strictEqual(rebuilt, true, 'предпосылка: изменившиеся данные обязаны пересобрать панель');
    editorA = pane._editors.find((e) => e._model === 'Qwen3.8-27B-UD-Q4_K_M');
    assert.ok(/32768/.test(editorA._cells.contextLength ? editorA._cells.contextLength.textContent : ''),
        'после пересборки панели значение профиля модели потерялось (появилось «не задано»)');
    assert.ok(editorA._limit && /предел/i.test(editorA._limit.textContent || ''),
        'после пересборки панели пропал «предел окна» (R319)');

    // СМЕНА МОДЕЛИ: у gemma-4 профиля нет — значения Qwen не должны «переехать».
    const editorB = pane._editors.find((e) => e._model === 'gemma-4-E4B-it-Q4_K_M');
    assert.ok(editorB, 'редактор gemma-4 не создан рендером');
    const bCell = editorB._cells.contextLength ? editorB._cells.contextLength.textContent : '';
    assert.ok(!/32768/.test(bCell),
        'кэш применился к ДРУГОЙ модели: gemma-4 показывает 32768 от Qwen3.8 (смена модели не инвалидирует кэш)');
    assert.ok(!/32768/.test(editorB._fields.contextLength ? String(editorB._fields.contextLength.value) : ''),
        'в поле другой модели подставлено чужое значение из кэша');
});

// --- (6) инвалидация: сохранение и явное обновление ------------------------

check('R691(е): после сохранения кэш не свежий, но содержимое не стирается', async function () {
    requireCache();
    M.state.detailPane = 'settings';
    M.refreshDetailPanel();
    await flush();
    const blocked = blockText('cppLoadDefaults');
    assert.ok(blocked && !isPlaceholder(blocked), 'предпосылка: блок дефолтов наполнен');
    assert.ok(formState && typeof formState.noteSaved === 'function', 'нет noteSaved');
    formState.noteSaved(pane);
    // Кэш помечен устаревшим: свежим он больше не читается.
    const fresh = Cache.read('cppLoadDefaults', {});
    assert.strictEqual(fresh, undefined, 'после сохранения noteSaved не инвалидировал кэш блоков');
    // ...но и не стёрт: свежая пересборка не показывает заглушку.
    server.version = 4;
    const rebuilt = M.refreshDetailPanel();
    assert.strictEqual(rebuilt, true, 'предпосылка: данные изменились');
    assert.ok(!isPlaceholder(blockText('cppLoadDefaults')),
        'после сохранения пересборка вернула заглушку — содержимое стёрто, а не помечено устаревшим');
    await flush();
});

check('R691(ж): авто-проход кэш не инвалидирует, явное «Обновить» — инвалидирует', async function () {
    requireCache();
    // Автоматический проход: данные обновляются, кэш остаётся свежим.
    M.state.detailPane = 'settings';
    Cache.write('probe', { ok: 1 }, {});
    global.Api.getBackend = function () { return Promise.resolve({ id: 'b1', backendType: 'llama_cpp', llamaCpp: { loadedModels: [] } }); };
    M.state.selectedBackendId = 'b1';
    M.state.registeredBackends = [{ id: 'b1', status: 'healthy' }];
    await M.refreshDetail({ auto: true });
    assert.notStrictEqual(Cache.read('probe', {}), undefined,
        'авто-проход инвалидировал кэш — блоки будут мигать заглушкой на каждом тике');
    // Явное действие оператора: кэш больше не свежий (ответ заменит его).
    Cache.write('probe', { ok: 2 }, {});
    await M.refreshDetail();
    assert.strictEqual(Cache.read('probe', {}), undefined,
        'явное «Обновить» не инвалидировало кэш асинхронных блоков');
});

check('R691(з): смена страницы стирает кэш, устаревший ответ не перебивает свежий', async function () {
    requireCache();
    global.App.state.currentPage = 'gguf';
    Cache.write('probe', { ok: 'gguf' }, {});
    assert.notStrictEqual(Cache.read('probe', {}), undefined, 'предпосылка: запись есть');
    global.App.state.currentPage = 'dashboard';
    assert.strictEqual(Cache.read('probe', {}, { allowStale: true }), undefined,
        'смена страницы не стёрла кэш: на возврате покажется содержимое прошлого визита');
    global.App.state.currentPage = 'gguf';

    // Порядок ответов: запрос, начатый до инвалидации, не имеет права перебить
    // свежий ответ, пришедший после неё.
    const t1 = Cache.begin('probe', {});
    assert.ok(t1, 'begin не выдал тикет');
    assert.strictEqual(Cache.begin('probe', {}), null, 'второй параллельный запрос того же блока не должен начинаться');
    Cache.invalidate('refresh');
    const t2 = Cache.begin('probe', {});
    assert.ok(t2, 'после инвалидации новый запрос не начался (старый держит блок)');
    assert.strictEqual(Cache.commit(t1, { ok: 'stale' }), false, 'устаревший ответ принят в кэш');
    assert.strictEqual(Cache.commit(t2, { ok: 'fresh' }), true, 'свежий ответ отвергнут');
    assert.deepStrictEqual(Cache.read('probe', {}), { ok: 'fresh' }, 'в кэше не свежие данные');
});

// --- (7) статическая обвязка -------------------------------------------------

check('R691(и): порядок скриптов, restore после пересборки, регистрация блоков', function () {
    const indexHtml = read('index.html');
    const refreshSrc = read('js/modules/gguf-renderer-refresh.js');
    const formStateSrc = read('js/modules/gguf-detail-form-state.js');
    const paramsSrc = read('js/modules/cppworker-params.js');
    const detailParamsSrc = read('js/modules/gguf-detail-params.js');
    const settingsSrc = read('js/modules/gguf-detail-settings.js');

    const cacheIdx = indexHtml.indexOf('js/modules/gguf-async-cache.js');
    const refreshIdx = indexHtml.indexOf('js/modules/gguf-renderer-refresh.js');
    const cppParamsIdx = indexHtml.indexOf('js/modules/cppworker-params.js');
    const detailParamsIdx = indexHtml.indexOf('js/modules/gguf-detail-params.js');
    assert.ok(cacheIdx > 0, 'index.html не подключает js/modules/gguf-async-cache.js');
    assert.ok(cacheIdx < refreshIdx,
        'модуль кэша подключён ПОСЛЕ refresh-модуля: restore после пересборки не сработает');
    assert.ok(cacheIdx < cppParamsIdx && cacheIdx < detailParamsIdx,
        'модуль кэша подключён ПОСЛЕ регистрирующих блоки модулей');

    assert.ok(/restoreAsyncBlocks\(node\)/.test(refreshSrc) && /GgufAsyncCache/.test(refreshSrc),
        'refresh-модуль не зовёт restore кэша после пересборки панели');
    assert.ok(/invalidate\('refresh'\)/.test(refreshSrc),
        'явное «Обновить» не инвалидирует кэш асинхронных блоков');
    assert.ok(/noteSaved\(/.test(formStateSrc) && /invalidate\('saved'\)/.test(formStateSrc),
        'noteSaved не инвалидирует кэш после сохранения');
    assert.ok(/invalidate\('reload'/.test(settingsSrc),
        'явный Reload параметров бэкенда не инвалидирует кэш блоков');
    ['cppProfiles', 'cppLoadDefaults', 'cppModelCatalog'].forEach(function (kind) {
        assert.ok(new RegExp("kind:\\s*'" + kind + "'").test(paramsSrc),
            'cppworker-params.js не регистрирует блок ' + kind);
    });
    assert.ok(/kind:\s*'modelProfile'/.test(detailParamsSrc),
        'gguf-detail-params.js не регистрирует блок параметров модели');
    assert.ok(/begin\(/.test(paramsSrc) && /commit\(/.test(paramsSrc),
        'загрузка блоков не использует тикеты begin/commit — устаревший ответ перебьёт свежий');
});

// --- прогон ------------------------------------------------------------------

(async function main() {
    for (let i = 0; i < checks.length; i++) {
        try {
            await checks[i][1]();
            passed++;
            console.log('  \u2713 ' + checks[i][0]);
        } catch (e) {
            failures.push(checks[i][0] + ': ' + (e && e.message));
            console.error('  \u2717 ' + checks[i][0] + ' — ' + (e && e.message));
        }
    }
    console.log('\n' + (failures.length ? 'FAILED: ' + failures.length : 'ИТОГ: все проверки пройдены (' + passed + ')'));
    if (failures.length) {
        failures.forEach(function (f) { console.error(' - ' + f); });
        process.exit(1);
    }
})();
