// webui/tests/form-dirty-guard.test.js — R662 (2026-11-20): незавершённый ввод
// защищает страницу от авто-обновления, а не только фокус в поле.
//
// ЖИВАЯ ЖАЛОБА (продолжение R512). «Постоянное обновление WebUI не даёт
// взаимодействовать с формами». R512 пропускал тик только пока document.activeElement —
// редактируемое поле активной страницы. Дырка: набрал значение → кликнул чекбокс или
// пустое место → фокус уже не в текстовом поле → следующий тик (раз в 5 с)
// перерисовывает страницу и затирает набранное.
//
// R676 (2026-11-21): ЗАСЛОН — НЕ ЕДИНСТВЕННАЯ ЗАЩИТА, И ОН НЕ ГЛУШИТ ДАННЫЕ.
//
// Третья редакция той же жалобы: «постоянное обновление стирает то, что я ввёл в
// формы». Замер на стенде: набрал 9999 → кликнул мимо → один refreshPage('settings')
// → в поле снова 2048, причем FormDirty.isDirtyPage('settings') === true. Значит
// защита «не перерисовывать» сработать не могла: значения заливаются напрямую.
// Теперь значения полей защищает SafeForm (js/modules/safe-form.js) по-полевому, а
// «грязная» страница «Настроек» больше не останавливает тик целиком — иначе
// вторая половина жалобы («обновление не показывает новое») оставалась бы живой.
//
// Проверяется:
//   (1) модуль отличает правку от клика и знает, к какой странице она относится;
//   (2) защита снимается сохранением (submit) и сама истекает по TTL;
//   (3) тик спрашивает FormDirty ДО перерисовки, а для страниц с SafeForm не
//       глушит данные (SAFE_FORM_PAGES);
//   (4) модуль подключён в index.html;
//   (5) ИНТЕГРАЦИЯ НА РЕАЛЬНЫХ МОДУЛЯХ (R676): form-dirty + safe-form +
//       app-settings + app-models + app-data поднимаются вместе, и проверяется
//       поведение, а не текст:
//       (а) набрал → blur → один цикл refreshPage('settings') → значение цело;
//       (б) данные продолжают обновляться, когда форма защищена;
//       (в) поле, которое оператор менял, защищено и БЕЗ «грязной» страницы
//           (TTL истёк / форма сохранена), а вернул базовое — снова управляется.
//   (6) ТИК АВТО-ОБНОВЛЕНИЯ (R678): при несохранённой правке (страница «грязная»,
//       фокус уже ушёл) тик обязан дойти до провайдеров «Настроек» и обновить ОБА
//       пути данных раздела — SettingsRegistry.load() и loadSettings() → Api.config,
//       не тронув поле оператора. Это ровно пункт «б) данные страницы обновляются
//       тиками» живой приёмки (check-forms.js). Заодно фиксируется обратное:
//       «грязная» страница БЕЗ SafeForm («Image-модели») по-прежнему глушит тик
//       (R662). R688: GGUF в этот список больше не входит — у него своя защита
//       правой панели (gguf-detail-form-state.js), и данные обязаны обновляться.
//
// Запуск: node webui/tests/form-dirty-guard.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const root = path.join(__dirname, '..');
const moduleSrc = fs.readFileSync(path.join(root, 'js', 'modules', 'form-dirty.js'), 'utf8');
const safeFormSrc = fs.readFileSync(path.join(root, 'js', 'modules', 'safe-form.js'), 'utf8');
const settingsSrc = fs.readFileSync(path.join(root, 'js', 'modules', 'app-settings.js'), 'utf8');
const modelsSrc = fs.readFileSync(path.join(root, 'js', 'modules', 'app-models.js'), 'utf8');
const appDataJs = fs.readFileSync(path.join(root, 'js', 'modules', 'app-data.js'), 'utf8');
const indexHtml = fs.readFileSync(path.join(root, 'index.html'), 'utf8');

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
async function acheck(name, fn) {
    try {
        await fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' — ' + (e && e.message));
    }
}

// Загружаем модуль так же, как в браузере: sandbox с window/document.
function loadModule() {
    const sandboxWindow = {};
    const listeners = {};
    const sandboxDocument = {
        addEventListener: function (type, fn) { listeners[type] = fn; },
        activeElement: null,
        body: { id: 'body', parentNode: null }
    };
    const sandbox = {
        window: sandboxWindow,
        document: sandboxDocument,
        Date: Date,
        module: { exports: {} }
    };
    vm.createContext(sandbox);
    vm.runInContext(moduleSrc, sandbox, { filename: 'form-dirty.js' });
    assert.ok(sandboxWindow.FormDirty, 'модуль form-dirty.js не выставил window.FormDirty');
    return { api: sandboxWindow.FormDirty, listeners, sandbox };
}

// Фейковый DOM-узел: pageIdOf достаточно id/parentNode/tagName/type.
function node(tagName, props) {
    return Object.assign({ tagName: tagName }, props || {});
}
function page(id) {
    return node('DIV', { id: id, parentNode: null });
}

// --- (1) правка внутри страницы против клика по кнопке ---
check('правка в поле страницы помечает страницу', () => {
    const { api } = loadModule();
    const settingsPage = page('settings-page');
    const input = node('INPUT', { type: 'text', id: 'cfg-host', parentNode: settingsPage });
    assert.strictEqual(api.isDirtyPage('settings'), false, 'до правки страница чистая');
    api.noteEdit(input);
    assert.strictEqual(api.isDirtyPage('settings'), true, 'после правки страница «грязная»');
    assert.strictEqual(api.isDirtyPage('settings-page'), true, 'форма с суффиксом -page тоже распознаётся');
});

check('клик по кнопке (не редактируемый элемент) страницу не помечает', () => {
    const { api } = loadModule();
    const settingsPage = page('settings-page');
    const button = node('BUTTON', { id: 'saveSettings', parentNode: settingsPage });
    assert.strictEqual(api.isEditable(button), false, 'кнопка не считается вводом');
    assert.strictEqual(api.isDirtyPage('settings'), false, 'без правки защиты нет');
});

check('правка вне страницы (нет контейнера -page) не помечает ничего', () => {
    const { api } = loadModule();
    const stray = node('INPUT', { type: 'text', id: 'search', parentNode: null });
    assert.strictEqual(api.pageIdOf(stray), '');
    assert.strictEqual(api.noteEdit(stray), '');
});

// --- (2) сброс защиты: submit и TTL ---
check('submit формы снимает защиту сразу', () => {
    const { api } = loadModule();
    const settingsPage = page('settings-page');
    const input = node('INPUT', { type: 'text', parentNode: settingsPage });
    api.noteEdit(input);
    assert.strictEqual(api.isDirtyPage('settings'), true);
    api.noteSave(settingsPage);
    assert.strictEqual(api.isDirtyPage('settings'), false, 'после сохранения защита не нужна');
});

check('защита истекает по TTL (авто-обновление возвращается само)', () => {
    const { api } = loadModule();
    const settingsPage = page('settings-page');
    const input = node('INPUT', { type: 'text', parentNode: settingsPage });
    let now = 1000;
    api.setNowForTest(() => now);
    api.setTTLForTest(5000);
    api.noteEdit(input);
    now = 1000 + 4000;
    assert.strictEqual(api.isDirtyPage('settings'), true, 'в пределах TTL защита действует');
    now = 1000 + 6000;
    assert.strictEqual(api.isDirtyPage('settings'), false, 'после TTL тик снова работает');
});

check('clearAll сбрасывает состояние (навигация по страницам)', () => {
    const { api } = loadModule();
    const settingsPage = page('settings-page');
    const input = node('INPUT', { type: 'text', parentNode: settingsPage });
    api.noteEdit(input);
    api.clearAll();
    assert.strictEqual(api.isDirtyPage('settings'), false);
});

// --- (3) тик автообновления реально спрашивает про несохранённый ввод ---
// Тело тика: от setInterval до `}, interval);`.
function tickBody() {
    const start = appDataJs.indexOf('refreshTimer = setInterval(');
    assert.ok(start !== -1, 'в app-data.js нет периодического цикла — проверка потеряла точку отсчёта');
    const end = appDataJs.indexOf('}, interval);', start);
    assert.ok(end !== -1, 'не найден конец тела периодического цикла');
    return appDataJs.slice(start, end);
}

check('тик спрашивает FormDirty ДО обращения к провайдерам страницы', () => {
    const tick = tickBody();
    const dirtyIdx = tick.indexOf('FormDirty.isDirtyPage');
    const refreshIdx = tick.indexOf('DR.refresh(');
    assert.ok(dirtyIdx > 0, 'в тике нет вызова FormDirty.isDirtyPage — защита не подключена');
    assert.ok(refreshIdx > 0, 'в тике не найден вызов DR.refresh(<страница>) — разметка тика изменилась');
    assert.ok(dirtyIdx < refreshIdx, 'проверка незавершённого ввода стоит ПОСЛЕ обращения к провайдерам');
});

check('R676: «грязная» страница с SafeForm не глушит обновление данных', () => {
    const tick = tickBody();
    assert.ok(/safeFormCoversPage\(page\)/.test(tick),
        'тик не спрашивает safeFormCoversPage: для «Настроек» данные снова встают на TTL, ' +
        'а значения полей защищает SafeForm');
    assert.ok(/var page = getCurrentPage\(\)/.test(tick),
        'страница тика обязана читаться геттером getCurrentPage() (R676: контракт страницы)');
    assert.ok(/SAFE_FORM_PAGES\s*=\s*\{\s*settings:\s*true/.test(appDataJs),
        'в SAFE_FORM_PAGES нет «Настроек» — страница из жалобы не покрыта безопасной записью полей');
    // R688: GGUF обязан быть в списке: иначе тик снова глушит ВСЮ страницу, пока
    // оператор заполняет параметры загрузки, и список моделей/статусы стоят.
    assert.ok(/SAFE_FORM_PAGES\s*=\s*\{[^}]*\bgguf:\s*true/.test(appDataJs),
        'GGUF не в SAFE_FORM_PAGES: «грязная» страница снова глушит обновление данных');
});

// --- (4) модуль подключён в index.html ---
check('index.html подключает form-dirty.js', () => {
    assert.ok(indexHtml.indexOf('js/modules/form-dirty.js') > 0,
        'index.html не подключает js/modules/form-dirty.js — window.FormDirty будет undefined');
    const guardIdx = indexHtml.indexOf('js/modules/refresh-guard.js');
    const dirtyIdx = indexHtml.indexOf('js/modules/form-dirty.js');
    assert.ok(guardIdx > 0 && dirtyIdx > guardIdx,
        'form-dirty.js должен идти ПОСЛЕ refresh-guard.js (логика страниц — единым блоком)');
});

// --- (5) ИНТЕГРАЦИЯ НА РЕАЛЬНЫХ МОДУЛЯХ (R676) ------------------------------
//
// ЗАЧЕМ НЕ В ИЗОЛЯЦИИ. Дефект жил не в одной функции, а в СВЯЗКЕ «фабрика ↔
// контракт страницы ↔ запись в поле»: app-data.js звал loadSettings (app-settings.js),
// тот заливал поля прямыми `el.value = …`, а реестр горячих настроек и таблица
// лимитов пересобирались целиком. Поэтому здесь поднимается настоящий стек:
// form-dirty.js + safe-form.js + app-settings.js + app-models.js + app-data.js.
// Фейковые — только DOM, сеть и состояние; вся логика настоящая.

function makeEl(id, props) {
    return Object.assign({
        id: id || '',
        tagName: 'DIV',
        parentNode: null,
        value: '',
        checked: false,
        innerHTML: '',
        textContent: '',
        style: {},
        dataset: {},
        classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
        addEventListener() {},
        removeEventListener() {},
        getAttribute() { return null; },
        setAttribute() {},
        querySelector() { return null; },
        querySelectorAll() { return []; },
        appendChild() {},
        removeChild() {},
        contains() { return false; }
    }, props || {});
}

/**
 * buildSettingsStack — собрать «Настройки» из настоящих модулей.
 *
 * @param {object} opts.serverConfig — что вернёт GET конфига (Api.config).
 */
function buildSettingsStack(opts) {
    const options = opts || {};
    let serverConfig = options.serverConfig || {};
    const elements = Object.create(null);
    const docListeners = {};
    const counters = { configGets: 0, badgeUpdates: 0 };
    // R678: стенд тика авто-обновления. Считаем ТРИ независимые вещи: дошёл ли тик
    // до DataRefresh.refresh(<страница>), исполнялись ли зарегистрированные
    // провайдеры и сколько раз реально загрузился реестр горячих настроек.
    // Именно эти счётчики читает живая приёмка (пункт «б»).
    const dr = { refreshCalls: [], providers: Object.create(null), registryLoads: 0 };
    let tickFn = null;
    let activeElement = null;

    // Страница, на которой стоит оператор. R678: переключаемая — на ней же
    // проверяется, что заслон R662 для страниц БЕЗ SafeForm остался.
    const pageName = options.page || 'settings';
    const settingsPage = makeEl(pageName + '-page');

    function field(id, props) {
        const el = makeEl(id, Object.assign({ parentNode: settingsPage }, props || {}));
        elements[id] = el;
        return el;
    }

    // Поля, которые заливает applyServerConfig (значения — как в index.html).
    field('ctxSize', { tagName: 'INPUT', type: 'number', value: '2048' });
    field('batchSize', { tagName: 'INPUT', type: 'number', value: '512' });
    field('gpuLayers', { tagName: 'INPUT', type: 'number', value: '-2' });
    field('modelAffinity', { tagName: 'INPUT', type: 'checkbox', checked: true });
    field('backendLimitsBody', { tagName: 'TBODY', innerHTML: '' });

    const documentStub = {
        readyState: 'complete',
        get activeElement() { return activeElement; },
        setActiveElement(el) { activeElement = el || null; },
        getElementById(id) {
            if (elements[id]) return elements[id];
            if (id === settingsPage.id) return settingsPage;
            return null;
        },
        querySelector() { return null; },
        querySelectorAll() { return []; },
        createElement() { return makeEl(''); },
        addEventListener(type, fn) { (docListeners[type] = docListeners[type] || []).push(fn); }
    };

    const storage = Object.create(null);
    const localStorageStub = {
        getItem(k) { return storage[k] === undefined ? null : storage[k]; },
        setItem(k, v) { storage[k] = String(v); },
        removeItem(k) { delete storage[k]; }
    };

    const Utils = {
        escapeHtml(s) { return String(s); },
        updateGpuLayersBadge() { counters.badgeUpdates++; },
        setText() {}
    };
    const I18N = { t(key) { return key; } };
    const api = {
        config() { counters.configGets++; return Promise.resolve(serverConfig); },
        handleError() {},
        updateBackendLimitsFull() { return Promise.resolve({ ok: true }); },
        getLlamaCppOverride() { return Promise.resolve(null); },
        proxyLogs() { return Promise.resolve({ entries: [] }); }
    };

    const sandboxWindow = {};
    // R678: двойники реестра обновлений и панели горячих настроек. Двойники
    // минимальные: тик спрашивает hasProvider/refresh, провайдер «Настроек» зовёт
    // SettingsRegistry.load() — ровно то, что читает живая приёмка.
    sandboxWindow.DataRefresh = {
        register(page, fn) {
            (dr.providers[page] = dr.providers[page] || []).push(fn);
            return true;
        },
        hasProvider(page) { return !!(dr.providers[page] && dr.providers[page].length); },
        isAuto() { return true; },
        markFresh() {},
        markError() {},
        refresh(page) {
            dr.refreshCalls.push(String(page));
            const list = dr.providers[page] || [];
            return Promise.all(list.map(function (fn) {
                return Promise.resolve().then(fn).then(function () { return true; }, function (e) { return e; });
            }));
        },
        _providers: dr.providers
    };
    sandboxWindow.SettingsRegistry = {
        load: function () { dr.registryLoads++; return Promise.resolve([]); }
    };
    const sandbox = {
        window: sandboxWindow,
        document: documentStub,
        localStorage: localStorageStub,
        Utils: Utils,
        I18N: I18N,
        Api: api,
        setTimeout(fn) { if (typeof fn === 'function') fn(); return 0; },
        clearTimeout() {},
        // R678: тик заводится настоящим startPeriodicRefresh — таймер фейковый,
        // тело тика настоящее (его и надо проверить).
        setInterval(fn) { tickFn = fn; return 1; },
        clearInterval() {},
        console: console,
        Date: Date,
        JSON: JSON,
        Promise: Promise
    };
    sandbox.globalThis = sandbox;
    vm.createContext(sandbox);
    vm.runInContext(moduleSrc, sandbox, { filename: 'form-dirty.js' });
    vm.runInContext(safeFormSrc, sandbox, { filename: 'safe-form.js' });
    vm.runInContext(settingsSrc, sandbox, { filename: 'app-settings.js' });
    vm.runInContext(modelsSrc, sandbox, { filename: 'app-models.js' });
    vm.runInContext(appDataJs, sandbox, { filename: 'app-data.js' });

    const noop = function () { return undefined; };
    const data = {
        backends: [{ id: 'b1', host: '10.0.0.1', status: 'healthy', maxConcurrentRequests: 3, maxModels: 2 }],
        sessions: [], queue: {}, queueTasks: [], queueHistory: [],
        logs: [], models: [], proxyLogs: [], agents: null
    };
    const getCurrentPage = function () { return pageName; };

    const settingsUi = sandboxWindow.AppSettings({
        data: data,
        getCurrentPage: getCurrentPage,
        refreshPage: noop,
        showToast: noop,
        updateConnectionStatus: noop,
        updateBackendEngineBadge: noop
    });
    const modelsUi = sandboxWindow.AppModels({
        Api: api,
        I18N: I18N,
        Utils: Utils,
        addLog: noop,
        backendsPage: noop,
        getCurrentPage: getCurrentPage,
        data: data,
        filterBackendsForUI: function (list) { return list; },
        renderLogs: noop,
        renderProxyLogs: noop,
        showToast: noop,
        fetchClusterState: function () { return Promise.resolve(); }
    });
    const dataUi = sandboxWindow.AppData({
        Api: api,
        I18N: I18N,
        UNHEALTHY_STATUSES_UI: {},
        Utils: Utils,
        addLog: noop,
        addLogEntry: noop,
        addProxyLog: noop,
        backendsPage: noop,
        getCurrentPage: getCurrentPage,
        dashboard: noop,
        data: data,
        exportBackends: noop,
        exportLogs: noop,
        exportProxyLogs: noop,
        init: noop,
        initLogsStream: noop,
        loadSettings: settingsUi.loadSettings,
        fetchClusterState: function () { return Promise.resolve(); },
        fetchQueue: function () { return Promise.resolve(); },
        fetchQueueDetails: function () { return Promise.resolve(); },
        fetchQueueHistory: function () { return Promise.resolve(); },
        fetchSessions: function () { return Promise.resolve(); },
        fetchAgents: function () { return Promise.resolve(); },
        fetchProxyLogs: noop,
        loadBackendLimits: modelsUi.loadBackendLimits,
        startOpsAutoRefresh: noop,
        stopOpsAutoRefresh: noop,
        modelsPage: noop,
        predictionAlerts: noop,
        queuePage: noop,
        renderAgentDetails: noop,
        renderAgentsPage: noop,
        renderLogs: noop,
        renderNotification: noop,
        renderNotificationsList: noop,
        renderProxyLogs: noop,
        sessionsPage: noop,
        setupNotificationsUI: noop
    });

    return {
        sandbox: sandbox,
        window: sandboxWindow,
        document: documentStub,
        elements: elements,
        counters: counters,
        dataApi: dataUi,
        loadSettings: settingsUi.loadSettings,
        setServerConfig(cfg) { serverConfig = cfg; },
        /** type — оператор набирает в поле и уводит фокус (клик мимо). */
        type(id, value) {
            const el = elements[id];
            assert.ok(el, 'в стенде нет поля #' + id);
            documentStub.setActiveElement(el);
            el.value = String(value);
            (docListeners.input || []).forEach(function (fn) { fn({ target: el }); });
            documentStub.setActiveElement(null);
            return el;
        },
        safeForm() { return sandboxWindow.SafeForm; },
        formDirty() { return sandboxWindow.FormDirty; },
        refresh() { return dataUi.refreshPage('settings'); },
        /** drRefreshCalls — с какими именами страниц тик звал провайдеров. */
        drRefreshCalls() { return dr.refreshCalls.slice(); },
        /** registryLoads — сколько раз исполнился SettingsRegistry.load(). */
        registryLoads() { return dr.registryLoads; },
        /**
         * startTicker — как app.js на старте: зарегистрировать провайдеров страниц
         * и завести периодический цикл (таймер подменён, тело тика настоящее).
         */
        startTicker() {
            dataUi.registerPageProviders();
            dataUi.startPeriodicRefresh();
            assert.ok(typeof tickFn === 'function', 'тик не завёлся: startPeriodicRefresh не создал таймер');
            return sandboxWindow.DataRefresh.hasProvider(pageName);
        },
        /** runTick — один цикл авто-обновления. */
        runTick() {
            assert.ok(typeof tickFn === 'function', 'нет тела тика: сначала startTicker()');
            tickFn();
        },
        /** markDirty — на странице есть свежая правка оператора (input/change). */
        markDirty(page) {
            sandboxWindow.FormDirty.noteEdit({
                tagName: 'INPUT', type: 'text', id: 'probe-' + page,
                parentNode: makeEl(page + '-page')
            });
        }
    };
}

/** flush — дождаться промисов загрузки конфига (Api.config → applyServerConfig). */
function flush() {
    return new Promise(function (resolve) { setImmediate(resolve); });
}

const SERVER_2048 = {
    modelAffinity: true,
    llamaCpp: { contextLength: 2048, batchSize: 512, numGpuLayers: -2 }
};

(async function integration() {
    // (а) набрал → blur → ОДИН цикл refreshPage('settings') → значение цело.
    await acheck('R676(а): набрал 9999 → кликнул мимо → refreshPage сохраняет ввод', async () => {
        const stack = buildSettingsStack({ serverConfig: SERVER_2048 });
        stack.loadSettings();
        await flush();
        assert.strictEqual(stack.elements.ctxSize.value, '2048', 'первый рендер должен залить серверное значение');

        stack.type('ctxSize', 9999);
        assert.strictEqual(stack.formDirty().isDirtyPage('settings'), true,
            'правка оператора обязана пометить страницу (на этом стоит половина защиты)');

        stack.refresh();
        await flush();
        assert.strictEqual(stack.elements.ctxSize.value, '9999',
            'refreshPage(' + "'settings'" + ') стёр набранное число — жалоба «обновление стирает формы» живая');
    });

    // (б) данные продолжают обновляться, когда форма защищена.
    await acheck('R676(б): защищённая форма не глушит рендер и данные', async () => {
        const stack = buildSettingsStack({ serverConfig: SERVER_2048 });
        stack.loadSettings();
        await flush();
        const configGetsBefore = stack.counters.configGets;
        const badgeBefore = stack.counters.badgeUpdates;
        stack.safeForm().resetStats();

        stack.type('ctxSize', 9999);
        stack.setServerConfig({ modelAffinity: true, llamaCpp: { contextLength: 4096, batchSize: 256, numGpuLayers: -2 } });
        stack.refresh();
        await flush();

        assert.strictEqual(stack.elements.ctxSize.value, '9999', 'ввод оператора потерян');
        assert.ok(stack.counters.configGets > configGetsBefore,
            'перерисовка настроек не запускалась вовсе — данные «встали» на TTL');
        assert.ok(stack.counters.badgeUpdates > badgeBefore,
            'рендерер не дошёл до конца: защита снова глушит страницу целиком, а не поле');
        const stats = stack.safeForm().stats();
        assert.ok(stats.blocked > 0, 'SafeForm не вызывался — поле защищено случайно, а не механизмом');
    });

    // (в) поле, которое оператор менял, защищено и без «грязной» страницы;
    //     соседние поля обновляются; вернул базовое — SafeForm снова управляет.
    await acheck('R676(в): после TTL защищено только поле оператора, соседние обновляются', async () => {
        const stack = buildSettingsStack({ serverConfig: SERVER_2048 });
        stack.loadSettings();
        await flush();

        stack.type('ctxSize', 9999);
        // TTL истёк / форму сохранили: страница больше не «грязная».
        stack.formDirty().clear('settings');
        assert.strictEqual(stack.formDirty().isDirtyPage('settings'), false, 'предпосылка: страница чистая');

        stack.setServerConfig({ modelAffinity: true, llamaCpp: { contextLength: 4096, batchSize: 256, numGpuLayers: -2 } });
        stack.safeForm().resetStats();
        stack.refresh();
        await flush();

        assert.strictEqual(stack.elements.ctxSize.value, '9999',
            'поле, которое оператор менял, перетёрто серверным значением после истечения TTL');
        assert.strictEqual(stack.elements.batchSize.value, '256',
            'соседнее поле не обновилось: защита поля превратилась в остановку рендера');
        assert.ok(stack.safeForm().stats().written > 0, 'ни одна запись не прошла — рендер не работал');
        assert.strictEqual(stack.elements.modelAffinity.checked, true, 'чекбокс не обновился из конфига');

        // Оператор вернул поле к базовому значению (тому, что последним записал
        // сам SafeForm — 2048) — с этого момента поле снова под его управлением.
        stack.elements.ctxSize.value = '2048';
        stack.setServerConfig({ modelAffinity: true, llamaCpp: { contextLength: 8192, batchSize: 256, numGpuLayers: -2 } });
        stack.refresh();
        await flush();
        assert.strictEqual(stack.elements.ctxSize.value, '8192',
            'после возврата к базовому значению поле «залипло» — управление SafeForm не вернулось');
    });

    // (д) R678: ТИК ПРИ НЕСОХРАНЁННОЙ ПРАВКЕ ОБЯЗАН ОБНОВИТЬ ДАННЫЕ РАЗДЕЛА.
    //
    // Живой дефект, ради которого кейс добавлен: приёмка «данные страницы
    // обновляются тиками» читает SettingsRegistry.load = 0. Причины в коде
    // бывают две, и обе означают «тик не дошёл до провайдеров страницы»:
    //   - заслон R662 («грязная» страница глушит тик целиком) — тогда данные
    //     раздела стоят до ручного обновления;
    //   - сломанный контракт страницы r675 (в тик уходила функция вместо имени)
    //     — тогда провайдер 'settings' не вызывался НИ РАЗУ.
    // Кейс проверяет поведение, а не текст: поднимает настоящий app-data.js,
    // заводит тик и требует, чтобы при isDirtyPage('settings') === true тик
    // вызвал ОБА пути данных раздела (Api.config через loadSettings и
    // SettingsRegistry.load), не тронув поле оператора.
    await acheck('R676(д): тик при несохранённой правке обновляет «Настройки» (оба пути)', async () => {
        const stack = buildSettingsStack({ serverConfig: SERVER_2048 });
        stack.loadSettings();
        await flush();
        assert.strictEqual(stack.elements.ctxSize.value, '2048', 'предпосылка: первый рендер залил серверное значение');
        const configGetsBefore = stack.counters.configGets;

        stack.type('ctxSize', 9999);
        assert.strictEqual(stack.formDirty().isDirtyPage('settings'), true, 'предпосылка: страница «грязная»');
        stack.safeForm().resetStats();

        assert.strictEqual(stack.startTicker(), true,
            'у «Настроек» нет провайдера в DataRefresh — тик не обновит её источники (пункт «б» приёмки)');
        stack.setServerConfig({ modelAffinity: true, llamaCpp: { contextLength: 4096, batchSize: 256, numGpuLayers: -2 } });
        stack.runTick();
        await flush();
        await flush();

        assert.ok(stack.drRefreshCalls().indexOf('settings') >= 0,
            'тик не дошёл до провайдеров «Настроек» (refresh вызван с: '
            + JSON.stringify(stack.drRefreshCalls()) + ') — данные раздела стоят до ручного обновления');
        assert.ok(stack.registryLoads() > 0,
            'тик не вызвал SettingsRegistry.load() — панель горячих настроек не обновляется тиками');
        assert.ok(stack.counters.configGets > configGetsBefore,
            'тик не вызвал loadSettings → Api.config — серверный конфиг не перечитывается');
        assert.strictEqual(stack.elements.ctxSize.value, '9999',
            'тик обновил данные, но заодно стёр ввод оператора: «данные» и «форма» — не «или/или»');
        assert.ok(stack.safeForm().stats().blocked > 0,
            'SafeForm не защищал поле — ввод уцелел случайно, а не механизмом');
    });

    // (е) R662 СОХРАНЁН для страниц БЕЗ покрытия: «грязная» страница без
    // SafeForm-защиты по-прежнему глушит тик целиком. R688 (2026-11-22): GGUF из
    // этого примера УШЁЛ — у страницы появилась собственная защита панели
    // (webui/js/modules/gguf-detail-form-state.js), и тик обязан продолжать
    // обновлять её данные. Поэтому пример — «Image-модели» (покрытия нет).
    await acheck('R662(е): «грязная» страница без SafeForm по-прежнему глушит тик', async () => {
        const stack = buildSettingsStack({ serverConfig: SERVER_2048, page: 'image' });
        stack.markDirty('image');
        assert.strictEqual(stack.formDirty().isDirtyPage('image'), true, 'предпосылка: страница «грязная»');
        stack.startTicker();
        stack.runTick();
        await flush();

        assert.deepStrictEqual(stack.drRefreshCalls(), [],
            'тик дошёл до провайдеров «грязной» image-страницы: заслон R662 для страниц без SafeForm снят');
        assert.strictEqual(stack.registryLoads(), 0, 'тик дёрнул реестр горячих настроек с чужой страницы');
        assert.strictEqual(stack.counters.configGets, 0, 'тик перечитал серверный конфиг на защищённой странице');
    });

    // (ж) R688: GGUF больше не глушится целиком. Пока в форме есть несохранённая
    // правка, данные страницы обязаны обновляться (список моделей, статусы), а
    // DOM правой панели защищает gguf-detail-form-state.js.
    await acheck('R688: «грязная» gguf-страница продолжает обновлять данные', async () => {
        const stack = buildSettingsStack({ serverConfig: SERVER_2048, page: 'gguf' });
        let providerCalls = 0;
        stack.window.DataRefresh.register('gguf', function () { providerCalls++; return Promise.resolve(); });
        stack.markDirty('gguf');
        assert.strictEqual(stack.formDirty().isDirtyPage('gguf'), true, 'предпосылка: страница «грязная»');
        stack.startTicker();
        stack.runTick();
        await flush();

        assert.deepStrictEqual(stack.drRefreshCalls(), ['gguf'],
            'тик не дошёл до провайдера «грязной» gguf-страницы — данные раздела снова стоят на TTL');
        assert.strictEqual(providerCalls, 1, 'провайдер gguf не вызван: список моделей и статусы не обновятся');
    });

    // (г) навигация снимает защиту только с покидаемой страницы (app.js).
    check('R676(г): switchPage снимает защиту только с покидаемой страницы', () => {
        const appJs = fs.readFileSync(path.join(root, 'js', 'app.js'), 'utf8');
        const start = appJs.indexOf('function switchPage(');
        assert.ok(start !== -1, 'в app.js нет switchPage');
        const body = appJs.slice(start, start + 2600);
        assert.ok(!/FormDirty\.clearAll\(/.test(body),
            'switchPage по-прежнему делает clearAll(): переход на другой раздел снимает защиту с ЧУЖИХ страниц');
        assert.ok(/FormDirty\.clear\(prevPage\)/.test(body),
            'switchPage не снимает защиту с покидаемой страницы (prevPage)');
    });

    if (failures.length) {
        console.error('\n' + failures.length + ' проверок провалено, ' + passed + ' пройдено');
        process.exit(1);
    }
    console.log('\nвсе ' + passed + ' проверок пройдены');
})();
