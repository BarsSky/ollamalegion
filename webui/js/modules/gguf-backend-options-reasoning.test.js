// gguf-backend-options-reasoning.test.js — R106b (2026-10-12).
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «не дало во вкладке настройки бекенда на странице gguf
// models поменять параметры размышления с выключенного на включенный», и «модель
// gemma4 при загрузке применяет не те параметры, что указаны в настройках».
//
// ЧТО ПРОВЕРЯЕТСЯ:
//   1. во вкладке настроек бэкенда появился блок «Что получат НОВЫЕ модели» и
//      «Модели в папке» (data-role) — раньше этот слой был виден только на
//      странице «Настройки», поэтому оператор правил настройки контейнера и не
//      видел, что при загрузке выигрывают профиль/настройки по умолчанию;
//   2. Save пишет reasoning не только в runtime-поле контейнера
//      (`enableReasoning`), но и в ПОЛЕ ЗАГРУЗКИ (`defaultEnableReasoning`) —
//      без второго модель наследует false, и «галочка не применяется»;
//   3. есть кнопка «Сделать приоритетом для новых моделей», и она пишет
//      load-affecting значения в слой «настройки по умолчанию»
//      (PUT /api/v1/cppworker/load-defaults) — тот, который применяется раньше
//      настроек контейнера.
//
// Запуск: node webui/js/modules/gguf-backend-options-reasoning.test.js

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const SRC = path.join(__dirname, 'gguf-renderer-detail-render.js');

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
            (this._listeners[type] || []).forEach(function (fn) { fn(ev || {}); });
        },
        getAttribute: function () { return null; },
        setAttribute: function () {},
        querySelector: function (sel) {
            if (sel && sel.length > 1 && sel[0] === '#') return getEl(sel.slice(1));
            return getEl('sel:' + sel);
        },
        querySelectorAll: function () { return []; },
        closest: function () { return null; },
        appendChild: function () {},
        remove: function () {},
        classList: { add: function () {}, remove: function () {}, contains: function () { return false; } },
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

const calls = { saveConfig: [], saveDefaults: [] };

const sandbox = {
    console: console,
    setTimeout: setTimeout,
    clearTimeout: clearTimeout,
    setInterval: function () { return 0; },
    clearInterval: function () {},
    MutationObserver: function () { this.observe = function () {}; this.disconnect = function () {}; },
};
sandbox.window = sandbox;
sandbox.document = {
    getElementById: getEl,
    querySelector: function (sel) {
        if (sel && sel.length > 1 && sel[0] === '#') return getEl(sel.slice(1));
        return getEl('sel:' + sel);
    },
    querySelectorAll: function () { return []; },
    createElement: function (tag) { return makeElement('created-' + tag); },
    body: { appendChild: function () {} },
    addEventListener: function () {},
};
sandbox.Utils = {
    escapeHtml: function (s) { return String(s == null ? '' : s); },
    updateGpuLayersBadge: function () {},
    setGpuLayers: function () {},
};
sandbox.I18N = { t: function (k, d) { return d || k; } };
sandbox.GgufApi = {
    requestViaBackend: function (backendId, url, opts) {
        if (url === '/api/v1/cppworker/config/update') {
            calls.saveConfig.push({ backendId: backendId, body: JSON.parse(opts.body) });
            return Promise.resolve({ status: 'updated', applied: ['enableReasoning'] });
        }
        if (url === '/api/v1/cppworker/config') {
            return Promise.resolve({ config: { defaultCtxSize: 32768, defaultBatchSize: 512, defaultGpuLayers: -1, defaultNFlashAttnType: -1, defaultUseMmap: true } });
        }
        return Promise.resolve({});
    },
    getRuntimeConfigViaBackend: function () { return Promise.resolve({ loaded_models: [] }); },
    getUrl: function () { return 'http://cppworker:18092'; },
};
sandbox.Api = {
    cppworkerLoadDefaults: {
        save: function (body) {
            calls.saveDefaults.push(body);
            return Promise.resolve({ status: 'ok', profile: body });
        },
    },
};

// Минимальный GgufModule: рендер берёт из него state и хелперы.
const ggufState = {
    selectedBackendId: 'cppworker-gpu-bundled-agent',
    registeredBackends: [
        { id: 'cppworker-gpu-bundled-agent', url: 'http://cppworker:18092', host: 'cppworker-gpu', cppWorkerPort: 18092 },
    ],
    _settingsFormHtmlByBackend: {},
    loadingModels: {},
    localModels: [],
    runtimeModels: {},
    // Настоящий state (gguf-renderer-state.js) всегда содержит loadOptions —
    // loadAndRenderBackendOptions синхронизирует его с конфигом бэкенда.
    loadOptions: {
        gpuLayers: -1,
        ctxSize: 2048,
        batchSize: 512,
        flashAttn: true,
        numa: false,
        useMmap: true,
        tensorSplit: null,
        autoGpuDistribution: true,
        strategy: 'vram-ratio',
    },
};
sandbox.window.GgufModule = {
    _: function (k, d) { return d || k; },
    showToast: function () {},
    stripGGUF: function (n) { return String(n || '').replace(/\.gguf$/i, ''); },
    formatFileSize: function (b) { return String(b); },
    formatVramMB: function (m) { return String(m); },
    // В браузере currentBackend даёт gguf-renderer-helpers.js; в тесте — тот же
    // контракт (текущий выбранный бэкенд из state).
    currentBackend: function () {
        return ggufState.registeredBackends.filter(function (b) {
            return b.id === ggufState.selectedBackendId;
        })[0] || null;
    },
    state: ggufState,
};

vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(SRC, 'utf8'), sandbox, { filename: SRC });
const M = sandbox.window.GgufModule;

(async function run() {
    check('вкладка настроек бэкенда показывает приоритетный слой (новые модели + каталог)', function () {
        const html = M.renderSettingsPane();
        assert.ok(/data-role="cpp-load-defaults"/.test(html),
            'нет блока «что получат новые модели» (data-role="cpp-load-defaults")');
        assert.ok(/data-role="cpp-model-catalog"/.test(html),
            'нет блока «модели в папке» (data-role="cpp-model-catalog")');
    });

    // Load options: форма рисуется в контейнер, обработчики вешаются на кнопки.
    getEl('ggufBackendOptionsContainer');
    await M.loadAndRenderBackendOptions();

    check('форма настроек бэкенда отрендерилась с кнопкой «Сделать приоритетом»', function () {
        const html = getEl('ggufBackendOptionsContainer').innerHTML;
        assert.ok(/id="ggufBackendOptionsSave"/.test(html),
            'форма настроек бэкенда не отрендерилась (ошибка загрузки конфига?): ' + html.slice(0, 300));
        assert.ok(/id="ggufBackendOptionsPromote"/.test(html),
            'нет кнопки «Сделать приоритетом для новых моделей»: ' + html.slice(0, 300));
        assert.ok(/id="ggufOptEnableReasoning"/.test(html),
            'нет галочки размышлений в форме: ' + html.slice(0, 300));
    });

    // Оператор включает размышления и сохраняет.
    getEl('ggufOptEnableReasoning').checked = true;
    getEl('ggufOptReasoningBudget').value = '2048';
    getEl('ggufOptCtxSize').value = '32768';
    getEl('ggufOptBatchSize').value = '512';
    getEl('ggufOptGpuLayers').value = '-1';
    getEl('ggufOptNParallel').value = '2';
    getEl('ggufOptKvCacheType').value = 'q4_0';
    getEl('ggufOptFlashAttn').value = '1';
    getEl('ggufOptUseMmap').checked = true;
    getEl('ggufBackendOptionsSave').dispatch('click');
    await new Promise(function (r) { setTimeout(r, 0); });

    check('Save пишет reasoning и в runtime-поле, и в поле загрузки', function () {
        assert.strictEqual(calls.saveConfig.length, 1,
            'PUT /api/v1/cppworker/config/update не вызван');
        const body = calls.saveConfig[0].body;
        assert.strictEqual(body.enableReasoning, true, 'enableReasoning не ушёл: ' + JSON.stringify(body));
        assert.strictEqual(body.defaultEnableReasoning, true,
            'defaultEnableReasoning не ушёл — новая загрузка унаследует false, и «галочка не применяется»: ' +
            JSON.stringify(body));
        assert.strictEqual(body.defaultReasoningBudget, 2048,
            'бюджет размышлений не ушёл в поле загрузки: ' + JSON.stringify(body));
    });

    // Кнопка «сделать приоритетом» пишет значения формы в приоритетный слой.
    // ВАЖНО про счётчик: в реальном DOM innerHTML-рендер уничтожает старые
    // элементы вместе с обработчиками, а в заглушке тот же объект копит
    // слушателей, поэтому считаем не «ровно один вызов», а прирост.
    const defaultsBefore = calls.saveDefaults.length;
    getEl('ggufBackendOptionsPromote').dispatch('click');
    await new Promise(function (r) { setTimeout(r, 0); });

    check('«Сделать приоритетом» пишет load-affecting значения в настройки по умолчанию', function () {
        assert.ok(calls.saveDefaults.length > defaultsBefore,
            'PUT /api/v1/cppworker/load-defaults не вызван кнопкой «Сделать приоритетом»');
        const body = calls.saveDefaults[calls.saveDefaults.length - 1];
        assert.strictEqual(body.contextLength, 32768, 'n_ctx не перенесён: ' + JSON.stringify(body));
        assert.strictEqual(body.numGpuLayers, -1, 'слои не перенесены: ' + JSON.stringify(body));
        assert.strictEqual(body.parallel, 2, 'параллельность не перенесена: ' + JSON.stringify(body));
        assert.strictEqual(body.kvCacheType, 'q4_0', 'KV-cache не перенесён: ' + JSON.stringify(body));
        assert.strictEqual(body.enableReasoning, true,
            'reasoning не перенесён в приоритетный слой: ' + JSON.stringify(body));
    });

    console.log('');
    if (failures.length) {
        console.error('gguf-backend-options-reasoning: ' + failures.length + ' FAILED, ' + passed + ' passed');
        failures.forEach(function (f) { console.error('  - ' + f); });
        process.exit(1);
    }
    console.log('gguf-backend-options-reasoning: OK (' + passed + ' checks passed)');
})();
