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
    // sandbox с рабочим RefreshGuard (ветка editing)
    const sandbox = { window: win, document: doc, console: { warn: function () {}, error: function () {} } };
    vm.createContext(sandbox);
    vm.runInContext(guardSrc, sandbox, { filename: 'refresh-guard.js' });
    const runner = new Function('serverConfig', 'window', 'document', 'console', 'localStorage', 'getCurrentPage',
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
function runLoadBackendLimits(editing) {
    const fn = extractFnIndented(modelsJs, 'loadBackendLimits');
    const tbody = { tagName: 'TBODY', innerHTML: 'СОХРАНЁННЫЙ_ВВОД', contains: function () { return editing; } };
    const doc = {
        activeElement: { tagName: 'INPUT', type: 'number' },
        getElementById: function (id) { return id === 'backendLimitsBody' ? tbody : null; }
    };
    const sandbox = { window: {}, document: doc };
    vm.createContext(sandbox);
    vm.runInContext(guardSrc, sandbox, { filename: 'refresh-guard.js' });
    const runner = new Function('window', 'document', 'data', 'Utils', 'I18N',
        fn.body + '\nreturn null;');
    runner(sandbox.window, doc, { backends: [{ id: 'b1', host: 'h', status: 'healthy' }] },
        { escapeHtml: function (s) { return String(s); } }, { t: function (k) { return k; } });
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
    assertGuardBefore(registrySrc, 'render', 'el.innerHTML = renderPanel()', 'settings-registry.js');
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
    assert.ok(/currentPage === 'settings'\) \{ return; \}/.test(tick[0]),
        'тик по-прежнему вызывает refreshPage(currentPage) для настроек — форма затирается повторно');
});

check('R513/4: кнопка «Обновить» и индикатор свежести идут тем же путём', () => {
    const cur = extractFn(appJs, 'refreshCurrentPage').body;
    assert.ok(/currentPage !== 'settings'/.test(cur),
        'refreshCurrentPage делает повторный refreshPage для настроек');
    const btn = appJs.slice(appJs.indexOf("document.getElementById('refreshBtn').addEventListener"));
    assert.ok(/currentPage !== 'settings'/.test(btn.slice(0, 2500)),
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

if (failures.length) {
    console.error('\nИТОГ: ' + failures.length + ' проверок красных');
    process.exit(1);
}
console.log('\nИТОГ: ' + passed + ' проверок зелёные');
