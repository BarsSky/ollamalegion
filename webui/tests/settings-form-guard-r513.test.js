// settings-form-guard-r513.test.js — R513 (2026-11-19): авто-обновление и кнопка
// «Обновить» не перетирают формы на странице «Настройки».
//
// ПРОДОЛЖЕНИЕ R512. R512 закрыл ровно один путь: таймер не запускал тик, пока
// курсор стоит в поле ввода (webui/tests/refresh-form-guard.test.js). Живой
// дефект остался по трём другим путям, и все три перерисовывают форму, даже
// когда оператор из неё уже вышел, но не сохранил:
//   1. refreshPage('settings') на каждом тике: loadSettings() → applyServerConfig
//      заливал ~40 полей из ответа сервера (набранное затиралось, курсор уезжал);
//   2. SettingsRegistry.load() → render() пересобирал ВСЮ панель горячих
//      настроек (`innerHTML`), а в ней поля ввода;
//   3. loadBackendLimits() пересобирал таблицу лимитов — `<input type="number">`
//      на каждый бэкенд;
//   4. AutoTuneSettings.render() пересобирал `<select>` на каждую модель.
//
// Проверки поведенческие там, где это возможно: тело функции вытаскивается из
// app.js и исполняется с подставным document — проверяется, что при вводе оно
// НИЧЕГО не заливает, а при отсутствии ввода работает как раньше.
const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const root = path.join(__dirname, '..');
const read = (rel) => fs.readFileSync(path.join(root, rel), 'utf8');
// R655: тик/refreshPage/registerPageProviders переехали в app-data.js.
const appJs = read('js/app.js') + '\n' + read('js/modules/app-data.js');
// R652: слой настроек (applyServerConfig и родня) вынесен в app-settings.js —
// тело берём оттуда; функции модуля на 8 пробелах, поэтому нужен терпимый extractor.
const settingsJs = read('js/modules/app-settings.js');
// R654: лимиты бэкендов переехали в app-models.js — тело берём оттуда.
const modelsJs = read('js/modules/app-models.js');
const guardSrc = read('js/modules/refresh-guard.js');
// R676: единственная точка записи в поля — её поведение и проверяется ниже.
const safeFormSrc = read('js/modules/safe-form.js');
const formDirtySrc = read('js/modules/form-dirty.js');
const registrySrc = read('js/modules/settings-registry.js');
const autotuneSrc = read('js/modules/autotune_settings.js');

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

// --- загрузка модуля refresh-guard в песочнице -------------------------------
function loadGuard(docActiveElement) {
    const sandboxWindow = {};
    const sandbox = {
        window: sandboxWindow,
        document: {
            activeElement: docActiveElement || null,
            getElementById: function () { return null; }
        }
    };
    vm.createContext(sandbox);
    vm.runInContext(guardSrc, sandbox, { filename: 'refresh-guard.js' });
    assert.ok(sandboxWindow.RefreshGuard, 'refresh-guard.js не выставил window.RefreshGuard');
    return sandboxWindow.RefreshGuard;
}

// --- извлечение тела функции по имени ---------------------------------------
// Функции в app.js объявлены на 4 пробелах, поэтому конец тела — следующий
// `\n    function ` (проверено: вложенных объявлений на этом отступе нет).
// R652: тот же extractor, но без требования 4-пробельного отступа (модуль — 8).
function extractFnIndented(src, name) {
    const start = src.indexOf('function ' + name + '(');
    assert.ok(start >= 0, 'в app-settings.js нет функции ' + name + ' — проверка потеряла точку отсчёта');
    const bodyOpen = src.indexOf('{', start);
    let depth = 0, i = bodyOpen;
    for (; i < src.length; i++) {
        if (src[i] === '{') depth++;
        else if (src[i] === '}' && --depth === 0) break;
    }
    return { head: src.slice(start, bodyOpen), body: src.slice(bodyOpen + 1, i) };
}

function extractFn(src, name) {
    const marker = '    function ' + name + '(';
    const start = src.indexOf(marker);
    assert.ok(start >= 0, 'в app.js нет функции ' + name + ' — проверка потеряла точку отсчёта');
    const bodyOpen = src.indexOf('{', start);
    assert.ok(bodyOpen > start, 'у функции ' + name + ' нет тела');
    let depth = 0;
    let i = bodyOpen;
    for (; i < src.length; i++) {
        const ch = src[i];
        if (ch === '{') depth++;
        else if (ch === '}') {
            depth--;
            if (depth === 0) break;
        }
    }
    assert.ok(i < src.length, 'не закрыто тело функции ' + name);
    return { head: src.slice(start, bodyOpen), body: src.slice(bodyOpen + 1, i) };
}

// --- поведение applyServerConfig --------------------------------------------
//
// R676 (2026-11-21): запись в поля идёт через SafeForm, поэтому в песочницу
// поднимается НАСТОЯЩИЙ модуль safe-form.js, а в runner добавляются реальные
// хелперы фабрики (writeValue/writeChecked/updateLlamaCppOverridesPanel), которые
// тело applyServerConfig зовёт по имени. Без них runner умирал бы
// ReferenceError-ом на последней строке, а проверки перед ней «зеленели».
function helpersSource(...names) {
    return names.map(function (name) {
        const fn = extractFnIndented(settingsJs, name);
        return fn.head + '{' + fn.body + '}';
    }).join('\n') + '\n';
}

function runApplyServerConfig(editing) {
    const fn = extractFnIndented(settingsJs, 'applyServerConfig');
    const requested = [];
    const writes = [];
    const settingsPage = { tagName: 'DIV', contains: function () { return editing; } };
    const doc = {
        activeElement: { tagName: 'INPUT', type: 'text' },
        getElementById: function (id) {
            requested.push(id);
            if (id === 'settings-page') return settingsPage;
            if (id === 'firstRunCard') return null; // R596: мастер заменён карточкой
            return {
                id: id,
                set value(v) { writes.push(id + '=' + v); },
                get value() { return ''; },
                set checked(v) { writes.push(id + '=' + v); },
                get checked() { return false; }
            };
        },
        querySelector: function () { return null; },
        querySelectorAll: function () { return []; }
    };
    const win = {};
    // sandbox с рабочим RefreshGuard (ветка editing) и SafeForm (ветка записи)
    const sandbox = { window: win, document: doc, console: { warn: function () {}, error: function () {} } };
    vm.createContext(sandbox);
    vm.runInContext(guardSrc, sandbox, { filename: 'refresh-guard.js' });
    vm.runInContext(safeFormSrc, sandbox, { filename: 'safe-form.js' });
    const runner = new Function('serverConfig', 'window', 'document', 'console', 'localStorage', 'getCurrentPage',
        helpersSource('writeValue', 'writeChecked', 'updateLlamaCppOverridesPanel') +
        fn.body + '\nreturn null;');
    let error = null;
    try {
        runner({}, win, doc, { warn: function () {}, error: function () {}, log: function () {} }, {
            setItem: function () {}, getItem: function () { return null; }
        }, function () { return 'settings'; });
    } catch (e) { error = e; }
    return { requested: requested, writes: writes, error: error };
}

check('R513/4: пока оператор в форме — applyServerConfig ничего не заливает', () => {
    const r = runApplyServerConfig(true);
    assert.deepStrictEqual(r.writes, [],
        'поля перезаписаны во время ввода: ' + r.writes.slice(0, 6).join(', '));
    assert.deepStrictEqual(r.requested, ['firstRunCard', 'settings-page'],
        'функция продолжила работу после guard-а (запрошены поля: ' + r.requested.join(', ') + ')');
});

check('R513/4: без ввода формы applyServerConfig заливает как раньше (guard не глушит всё)', () => {
    const r = runApplyServerConfig(false);
    assert.ok(r.requested.indexOf('modelAffinity') >= 0,
        'при отсутствии ввода поля не читаются вовсе — guard сломал обновление настроек');
    assert.ok(r.writes.length > 0, 'при отсутствии ввода серверные значения не применяются');
});

// --- поведение loadBackendLimits --------------------------------------------
// R676: пересборка строк вынесена в replaceRowsPreserving (перенос значений
// оператора через SafeForm), поэтому его тело тоже поднимается в runner.
function runLoadBackendLimits(editing) {
    const fn = extractFnIndented(modelsJs, 'loadBackendLimits');
    const repl = extractFnIndented(modelsJs, 'replaceRowsPreserving');
    const tbody = { tagName: 'TBODY', innerHTML: 'СОХРАНЁННЫЙ_ВВОД', contains: function () { return editing; } };
    const doc = {
        activeElement: { tagName: 'INPUT', type: 'number' },
        getElementById: function (id) { return id === 'backendLimitsBody' ? tbody : null; }
    };
    const sandbox = { window: {}, document: doc };
    vm.createContext(sandbox);
    vm.runInContext(guardSrc, sandbox, { filename: 'refresh-guard.js' });
    // SafeForm намеренно НЕ поднимается: проверяется ветка «модуль недоступен»
    // (прежнее поведение), а перенос значений проверяют интеграционные кейсы R676.
    const replaceFn = new Function('tbody', 'rowsHtml', 'window', repl.body);
    const runner = new Function('window', 'document', 'data', 'Utils', 'I18N', 'replaceRowsPreserving',
        fn.body + '\nreturn null;');
    runner(sandbox.window, doc, { backends: [{ id: 'b1', host: 'h', status: 'healthy' }] },
        { escapeHtml: function (s) { return String(s); } }, { t: function (k) { return k; } },
        function (tb, html) { return replaceFn(tb, html, sandbox.window); });
    return tbody;
}

check('R513/4: лимиты бэкендов не пересобираются, пока в них вводят', () => {
    const tbody = runLoadBackendLimits(true);
    assert.strictEqual(tbody.innerHTML, 'СОХРАНЁННЫЙ_ВВОД',
        'таблица лимитов перерисована во время ввода — набранное число потеряно');
});

check('R513/4: без ввода таблица лимитов обновляется', () => {
    const tbody = runLoadBackendLimits(false);
    assert.ok(/backend-limit-input/.test(tbody.innerHTML), 'таблица лимитов перестала обновляться');
});

// --- R676: пересборка таблицы лимитов НЕ теряет ввод после blur -------------
//
// Живой сценарий жалобы: набрал число в таблице → кликнул мимо (фокус ушёл) →
// пришёл тик → `tbody.innerHTML = …` уничтожил строку вместе с числом. Заслон
// R513 спасает только «курсор ещё в таблице». Здесь поднимается настоящий
// loadBackendLimits + настоящий SafeForm над таблицей, которая пересоздаёт поля
// так же, как браузер после подмены innerHTML.

function helpersFromModels(...names) {
    return names.map(function (name) {
        const fn = extractFnIndented(modelsJs, name);
        return fn.head + '{' + fn.body + '}';
    }).join('\n') + '\n';
}

/** Мини-таблица: подмена innerHTML пересоздаёт поля по разметке строк. */
function buildLimitsTable() {
    const inputs = [];
    const el = {
        tagName: 'TBODY',
        contains: function () { return false; },
        querySelectorAll: function () { return inputs.slice(); }
    };
    let html = '';
    Object.defineProperty(el, 'innerHTML', {
        get: function () { return html; },
        set: function (v) {
            html = String(v);
            inputs.length = 0;
            const re = /<input[^>]*data-backend-id="([^"]*)"[^>]*data-limit-type="([^"]*)"[^>]*value="([^"]*)"[^>]*>/g;
            let m;
            while ((m = re.exec(html)) !== null) {
                const bid = m[1], limitType = m[2], value = m[3];
                inputs.push({
                    tagName: 'INPUT', type: 'number', value: value, checked: false,
                    parentNode: { id: 'settings-page', tagName: 'DIV', parentNode: null },
                    getAttribute: function (n) {
                        if (n === 'data-backend-id') return bid;
                        if (n === 'data-limit-type') return limitType;
                        return null;
                    }
                });
            }
        }
    });
    return { el: el, inputs: inputs };
}

/** runLimitsRebuild — один проход refresh-а таблицы на настоящих модулях. */
function makeLimitsRebuild(table) {
    const load = extractFnIndented(modelsJs, 'loadBackendLimits');
    const repl = extractFnIndented(modelsJs, 'replaceRowsPreserving');
    const doc = {
        activeElement: null, // оператор уже кликнул мимо
        getElementById: function (id) { return id === 'backendLimitsBody' ? table.el : null; }
    };
    const sandbox = { window: {}, document: doc };
    vm.createContext(sandbox);
    vm.runInContext(guardSrc, sandbox, { filename: 'refresh-guard.js' });
    vm.runInContext(safeFormSrc, sandbox, { filename: 'safe-form.js' });
    const replaceFn = new Function('tbody', 'rowsHtml', 'window',
        helpersFromModels('limitRowKey') + repl.body + '\nreturn true;');
    const runner = new Function('window', 'document', 'data', 'Utils', 'I18N', 'replaceRowsPreserving',
        load.body + '\nreturn null;');
    return function rebuild(backends) {
        runner(sandbox.window, doc, { backends: backends },
            { escapeHtml: function (s) { return String(s); } }, { t: function (k) { return k; } },
            function (tb, html) { return replaceFn(tb, html, sandbox.window); });
    };
}

check('R676: набранное в таблице лимитов переживает пересборку (blur, потом тик)', () => {
    const table = buildLimitsTable();
    const rebuild = makeLimitsRebuild(table);
    rebuild([{ id: 'b1', host: 'h', status: 'healthy', maxConcurrentRequests: 3, maxModels: 2 }]);

    const conc = table.inputs.filter(function (i) { return i.getAttribute('data-limit-type') === 'maxConcurrent'; })[0];
    assert.ok(conc, 'в таблице нет поля maxConcurrent — проверка потеряла точку отсчёта');
    assert.strictEqual(conc.value, '3', 'предпосылка: поле заполнено серверным значением');

    conc.value = '77'; // оператор набрал 77 и ушёл из поля
    rebuild([{ id: 'b1', host: 'h', status: 'warning', maxConcurrentRequests: 5, maxModels: 4 }]);

    const after = table.inputs.filter(function (i) { return i.getAttribute('data-limit-type') === 'maxConcurrent'; })[0];
    const models = table.inputs.filter(function (i) { return i.getAttribute('data-limit-type') === 'maxModels'; })[0];
    assert.strictEqual(after.value, '77',
        'пересборка таблицы стёрла набранное число — «обновление стирает формы» живая жалоба');
    assert.strictEqual(models.value, '4',
        'соседнее поле не обновилось: защита одного поля остановила рендер');
    assert.ok(/badge-warning/.test(table.el.innerHTML),
        'статус бэкенда не обновился: данные таблицы перестали обновляться');
});

// --- статические проверки остальных трёх панелей ----------------------------

// guardBefore — guard стоит ДО перерисовки в указанной функции.
function assertGuardBefore(src, fnName, renderAnchor, fileLabel) {
    const fn = extractFn2(src, fnName);
    const guardPos = fn.indexOf('RefreshGuard');
    const renderPos = fn.indexOf(renderAnchor);
    assert.ok(guardPos !== -1, fileLabel + ': в ' + fnName + ' нет проверки RefreshGuard');
    assert.ok(renderPos !== -1, fileLabel + ': в ' + fnName + ' нет «' + renderAnchor + '» — проверка выродилась');
    assert.ok(guardPos < renderPos, fileLabel + ': проверка ввода стоит ПОСЛЕ перерисовки ' + fnName);
}

// extractFn2 — то же, но без требования 4-пробельного отступа (модули пишут на 6).
function extractFn2(src, name) {
    const marker = 'function ' + name + '(';
    const start = src.indexOf(marker);
    assert.ok(start >= 0, 'нет функции ' + name);
    const open = src.indexOf('{', start);
    let depth = 0;
    let i = open;
    for (; i < src.length; i++) {
        if (src[i] === '{') depth++;
        else if (src[i] === '}' && --depth === 0) break;
    }
    return src.slice(start, i + 1);
}

check('R513/4: панель горячих настроек не пересобирается во время ввода', () => {
    // R676: пересборка панели идёт через swapPanel(el, renderPanel()) — заслон
    // обязан стоять ДО неё (и до innerHTML внутри swapPanel).
    assertGuardBefore(registrySrc, 'render', 'swapPanel(el, renderPanel())', 'settings-registry.js');
});

check('R513/4: список моделей AutoTune не пересобирается во время выбора', () => {
    assertGuardBefore(autotuneSrc, 'renderPerModelList', 'list.innerHTML', 'autotune_settings.js');
});

check('R513/4: провайдер «Настроек» тянет ВСЕ источники страницы одним проходом', () => {
    const fn = extractFn(appJs, 'registerPageProviders').body;
    const settings = /DR\.register\('settings',[\s\S]{0,600}?\}\);/.exec(fn);
    assert.ok(settings, 'в registerPageProviders нет провайдера settings');
    ['loadSettings()', 'SettingsRegistry.load()', 'loadBackendLimits()'].forEach(function (needle) {
        assert.ok(settings[0].indexOf(needle) !== -1,
            'провайдер settings не вызывает ' + needle + ' — источник не обновится по кнопке/тику');
    });
});

check('R513/4: тик не делает второй проход refreshPage по «Настройкам»', () => {
    const tick = /refreshTimer = setInterval\([\s\S]*?\n        \}, interval\);/.exec(appJs);
    assert.ok(tick, 'не найдено тело периодического цикла');
    // R676: страница тика — значение из геттера, поэтому сравнение идёт по `page`.
    assert.ok(/(?:page|currentPage) === 'settings'\) \{ return; \}/.test(tick[0]),
        'тик по-прежнему вызывает refreshPage(<страница>) для настроек — форма затирается повторно');
    assert.ok(/var page = getCurrentPage\(\)/.test(tick[0]),
        'страница тика не читается геттером getCurrentPage() — сравнение с ' +
        "'settings' снова станет сравнением функции со строкой (дефект R676)");
});

check('R513/4: кнопка «Обновить» и индикатор свежести идут тем же путём', () => {
    const cur = extractFn(appJs, 'refreshCurrentPage').body;
    // R676: значение страницы снято в `page` в начале прохода.
    assert.ok(/(?:page|currentPage) !== 'settings'/.test(cur),
        'refreshCurrentPage делает повторный refreshPage для настроек');
    const btn = appJs.slice(appJs.indexOf("document.getElementById('refreshBtn').addEventListener"));
    assert.ok(/(?:page|currentPage) !== 'settings'/.test(btn.slice(0, 2500)),
        'кнопка «Обновить» делает повторный refreshPage для настроек');
});

check('R513/4: guard доступен по id (editingInId) — им пользуется applyServerConfig', () => {
    const g = loadGuard({ tagName: 'INPUT', type: 'text' });
    const fakePage = { tagName: 'DIV', contains: function (el) { return el === g.__active; } };
    assert.strictEqual(typeof g.editingInId, 'function', 'у RefreshGuard нет editingInId');
    // document.getElementById('x') возвращает null → guard обязан вернуть false, а не упасть
    assert.strictEqual(g.editingInId('settings-page'), false,
        'editingInId на отсутствующем элементе должен быть false');
});

// --- R676: поведение на СТАБИЛЬНОМ DOM (настоящие SafeForm + FormDirty) ------
//
// Зачем отдельно от runApplyServerConfig выше. Там `getElementById` возвращает
// НОВЫЙ объект на каждый вызов — так проверялся только текст («гард стоит до
// заливки»). Для поведения нужны постоянные узлы: SafeForm помнит базовое
// значение в WeakMap ПО УЗЛУ, а FormDirty — страницу по parentNode.
//
// Сценарий из живой жалобы: оператор набрал число → кликнул мимо (фокус ушёл) →
// пришёл тик → applyServerConfig заливает серверное значение. До R676 набранное
// исчезало.
function buildLiveSettingsDom() {
    const elements = {};
    const page = { id: 'settings-page', tagName: 'DIV', parentNode: null, contains: function () { return false; } };
    function field(id, type, value, checked) {
        const el = { id: id, tagName: 'INPUT', type: type || 'number', value: value, checked: !!checked, parentNode: page };
        elements[id] = el;
        return el;
    }
    field('ctxSize', 'number', '2048');
    field('batchSize', 'number', '512');
    field('gpuLayers', 'number', '-2');
    field('modelAffinity', 'checkbox', '', true);
    const doc = {
        activeElement: null, // оператор уже кликнул мимо: фокус не в поле
        getElementById(id) {
            if (id === 'settings-page') return page;
            if (id === 'firstRunCard') return null; // R596: мастер заменён карточкой
            return elements[id] || null;
        },
        querySelector() { return null; },
        querySelectorAll() { return []; }
    };
    return { elements: elements, page: page, doc: doc, counters: { badges: 0 } };
}

/**
 * buildApplyServerConfigStack — ОДИН раз поднять песочницу (RefreshGuard +
 * FormDirty + SafeForm + тело applyServerConfig), чтобы несколько проходов
 * обновления работали по одному и тому же состоянию: SafeForm помнит базу в
 * WeakMap, FormDirty — правку страницы. Новая песочница на каждый проход
 * обнуляла бы защиту и проверка «набранное цело» ничего бы не доказывала.
 */
function buildApplyServerConfigStack(dom) {
    const fn = extractFnIndented(settingsJs, 'applyServerConfig');
    const utils = {
        escapeHtml: function (s) { return String(s); },
        updateGpuLayersBadge: function () { dom.counters.badges++; },
        setText: function () {}
    };
    const sandbox = { window: {}, document: dom.doc, Utils: utils, I18N: { t: function (k) { return k; } },
        console: { warn: function () {}, error: function () {} } };
    vm.createContext(sandbox);
    vm.runInContext(guardSrc, sandbox, { filename: 'refresh-guard.js' });
    vm.runInContext(formDirtySrc, sandbox, { filename: 'form-dirty.js' });
    vm.runInContext(safeFormSrc, sandbox, { filename: 'safe-form.js' });
    const runner = new Function('serverConfig', 'window', 'document', 'console', 'localStorage', 'getCurrentPage',
        'Utils', 'I18N',
        helpersSource('writeValue', 'writeChecked', 'updateLlamaCppOverridesPanel') +
        fn.body + '\nreturn null;');
    return {
        window: sandbox.window,
        apply(serverConfig) {
            runner(serverConfig, sandbox.window, dom.doc,
                { warn: function () {}, error: function () {}, log: function () {} },
                { setItem: function () {}, getItem: function () { return null; } },
                function () { return 'settings'; }, utils, sandbox.I18N);
        }
    };
}

const SERVER_2048 = { modelAffinity: true, llamaCpp: { contextLength: 2048, batchSize: 512, numGpuLayers: -2 } };
const SERVER_4096 = { modelAffinity: true, llamaCpp: { contextLength: 4096, batchSize: 256, numGpuLayers: -2 } };

check('R676(а): набрал → кликнул мимо → applyServerConfig НЕ затирает ввод', () => {
    const dom = buildLiveSettingsDom();
    const stack = buildApplyServerConfigStack(dom);
    stack.apply(SERVER_2048); // первый рендер: поля получают базовое значение
    assert.strictEqual(dom.elements.ctxSize.value, '2048', 'предпосылка: первая заливка применилась');

    // Оператор набрал 9999 и кликнул мимо: фокус ушёл, страница помечена правкой.
    dom.elements.ctxSize.value = '9999';
    stack.window.FormDirty.noteEdit(dom.elements.ctxSize);
    assert.strictEqual(stack.window.FormDirty.isDirtyPage('settings'), true, 'предпосылка: страница помечена правкой');

    stack.window.SafeForm.resetStats();
    dom.counters.badges = 0;
    stack.apply(SERVER_4096);

    assert.strictEqual(dom.elements.ctxSize.value, '9999',
        'набранное значение стёрто обновлением — это и есть живая жалоба «обновление стирает формы»');
    assert.ok(dom.counters.badges > 0,
        'рендер не дошёл до конца: защита поля снова превратилась в остановку страницы');
    assert.ok(stack.window.SafeForm.stats().blocked > 0,
        'запись не доходила до SafeForm — поле сохранилось случайно, а не механизмом');
});

check('R676(б): данные обновляются, как только форма перестала быть «грязной»', () => {
    const dom = buildLiveSettingsDom();
    const stack = buildApplyServerConfigStack(dom);
    stack.apply(SERVER_2048);
    dom.elements.ctxSize.value = '9999';
    stack.window.FormDirty.noteEdit(dom.elements.ctxSize);
    // Форму сохранили (или истёк TTL) — страница чистая, но поле оператор менял.
    stack.window.FormDirty.clear('settings');
    stack.window.SafeForm.resetStats();

    stack.apply(SERVER_4096);

    assert.strictEqual(dom.elements.ctxSize.value, '9999',
        'поле, которое оператор менял, перетёрто после снятия «грязного» флага');
    assert.strictEqual(dom.elements.batchSize.value, '256',
        'соседнее поле не обновилось: защита одного поля остановила весь рендер');
    assert.ok(stack.window.SafeForm.stats().written > 0,
        'SafeForm не выполнил ни одной записи — данные страницы снова «встали»');
});

if (failures.length) {
    console.error('\nИТОГ: ' + failures.length + ' проверок красных');
    process.exit(1);
}
console.log('\nИТОГ: ' + passed + ' проверок зелёные');
