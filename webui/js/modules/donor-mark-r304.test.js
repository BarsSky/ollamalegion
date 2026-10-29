// donor-mark-r304.test.js — R304 (2026-10-30): «донор держит только память».
//
// Запуск: node webui/js/modules/donor-mark-r304.test.js
//
// ДОКТРИНА ОПЕРАТОРА (дословно): «Если бэкенд становится донором, он уже не
// может подымать свои копии моделей — он становится именно донором мастера и
// может только делить память с мастером».
//
// ЧТО БЫЛО НЕ ТАК ДО ЭТОЙ ПРАВКИ. Роль donor жила ТОЛЬКО в панели раскладки
// (js/monitor/layout-panel.js): на странице «Бэкенды» и на дашборде донор
// выглядел обычным здоровым узлом, а на странице «Модели» у него была живая
// кнопка «Загрузить». Живой класс дефектов: оператор видел «донор» на одной
// странице и загруженную копию на другой — то есть противоречие в интерфейсе
// вместо нарушения роли (см. internal/balancer/donor_exclusive_r304.go).
//
// Тест фиксирует ровно поведение панели (без сети и браузера):
//   1. донор с donor:true + donorFor{backendId} → бейдж с мастером в строке
//      списка и пояснение в раскрытой строке;
//   2. донор с ownCopies:['gemma'] → предупреждение и бейдж «своя копия: 1»;
//   3. донор БЕЗ donorFor.backendId → «мастер не назначен», без падения;
//   4. не-донор (main/standalone) → бейджа нет, кнопка load активна (регресс);
//   5. старый балансер (нет donor/canLoadOwnModels, только role:'donor') →
//      пометка всё равно есть (fallback работает);
//   6. executeModelOperation('load'|'pull') у донора → запрос в API НЕ ушёл.

'use strict';

const assert = require('assert');

// --- минимальный DOM ---------------------------------------------------------
// renderers.js на этапе загрузки вешает document.addEventListener и использует
// Utils.setText/setHTML/getElementById; Utils.escapeHtml создаёт элемент.
function makeEl(id) {
    const el = {
        id: id || '',
        innerHTML: '',
        textContent: '',
        value: '',
        hidden: false,
        style: {},
        dataset: {},
        children: [],
        classList: { add() {}, remove() {}, contains() { return false; } },
        setAttribute() {},
        getAttribute() { return null; },
        appendChild() {},
        removeChild() {},
        remove() {},
        addEventListener() {},
        querySelector() { return null; },
        querySelectorAll() { return []; },
    };
    return el;
}

const byId = {};
global.document = {
    createElement() {
        let text = '';
        const el = makeEl('');
        Object.defineProperty(el, 'textContent', { get() { return text; }, set(v) { text = String(v); } });
        Object.defineProperty(el, 'innerHTML', {
            get() {
                return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
            },
        });
        return el;
    },
    getElementById(id) {
        // toastContainer НЕ создаём: иначе showToast заводит setTimeout на 4 с и
        // держит процесс живым после последней проверки. Отсутствие контейнера
        // для showToast — штатная ветка «вернуть тихо» (app.js:2186).
        if (id === 'toastContainer') return null;
        return byId[id] || (byId[id] = makeEl(id));
    },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    addEventListener() {},
    removeEventListener() {},
    body: { appendChild() {} },
};
global.window = global;

// --- i18n --------------------------------------------------------------------
// Тексты — как в ru.js/en.js с ПОДСТАНОВКАМИ: если ключ пропал из словаря,
// проверка на текст упадёт (в UI оператор увидел бы сырой ключ).
const I18N_MAP = {
    'backends.donor_badge': 'донор (мастер: {master})',
    'backends.donor_badge_no_master': 'донор (мастер не назначен)',
    'backends.donor_own_copies': 'своя копия: {count}',
    'backends.donor_section': 'Донор: только память мастера',
    'backends.donor_detail': 'Донор: отдаёт VRAM мастеру {master} через rpc-server; свои модели не грузит',
    'backends.donor_detail_no_master': 'Донор: мастер не назначен — VRAM отдаётся через rpc-server; свои модели не грузит',
    'backends.donor_violation': 'у донора загружена своя копия: {names} — нарушение роли, выгрузите её или снимите роль',
    'models.donor_load_blocked': 'Донор не поднимает свои копии моделей: его VRAM отдана мастеру через rpc-server.',
    'models.load': 'Загрузить',
    'models.unload': 'Выгрузить',
    'models.delete': 'Удалить',
    'models.load_hint': 'Загрузить модель в память',
    'models.unload_hint': 'Выгрузить модель из памяти',
    'models.memory_title': 'Память',
    'backends.excluded': 'исключён',
    'backends.excluded_hint': 'исключён из выбора балансером',
    'renderers.digest': 'Digest',
    'renderers.expires': 'Истекает',
    // Панель раскладки (js/monitor/layout-panel.js): чип донора обязан называть
    // мастера — эти ключи из ru.js, чтобы проверять РЕАЛЬНУЮ подпись.
    'monitor.layout.donorShort': 'донор',
    'monitor.layout.donorMaster': 'мастер',
    'monitor.layout.donorNoMaster': 'мастер не назначен',
    'monitor.layout.donorTitle': 'Этот узел отдаёт свои слои главному по RPC',
    'monitor.layout.localShort': 'локально',
    'monitor.layout.localTitle': 'Раскладка не используется — считает только эта машина',
    'monitor.role.none': '— не задана —',
    'monitor.role.standalone': 'самодостаточный',
    'monitor.role.main': 'главный (раскладка)',
    'monitor.role.donor': 'донор (узел)',
    'monitor.role.title': 'Роль бэкенда в раскладке слоёв',
};

function tr(key, vars) {
    var s = Object.prototype.hasOwnProperty.call(I18N_MAP, key) ? I18N_MAP[key] : key;
    if (vars && typeof vars === 'object') {
        Object.keys(vars).forEach(function (k) {
            s = s.split('{' + k + '}').join(String(vars[k]));
        });
    }
    return s;
}

global.window.I18N = { t: tr, getLang() { return 'ru'; } };

require('./utils.js');
global.Utils = global.window.Utils;
require('./renderers.js');
const R = global.window.Renderers;
assert.ok(R, 'window.Renderers должен быть экспортирован');

// --- подставной API + загрузка app.js ---------------------------------------
// app.js сам по себе исполняется в node (проверено): на этапе загрузки он только
// разбирает Renderers и регистрирует DOMContentLoaded.
const apiCalls = [];
global.Api = {
    backendModelOperation(backendId, operation, modelName, options) {
        apiCalls.push({ backendId, operation, modelName, options });
        return Promise.resolve({ success: false, error: 'stub' });
    },
    backendModels() { return Promise.resolve({ models: [] }); },
};
global.BackendCRUD = { editBackend() {}, confirmDeleteBackend() {} };

// Список бэкендов, который в живой панели лежит в App.context.data (тот же
// объект, что data в app.js) — app.js берёт роль оттуда, без нового запроса.
const BACKENDS = [];
global.App = { context: { data: { backends: BACKENDS } } };

require('../app.js');
const ui = global.window.App.ui;
assert.ok(ui && typeof ui.executeModelOperation === 'function',
    'app.js должен опубликовать ui.executeModelOperation');

let passed = 0;
function check(name, fn) {
    fn();
    passed++;
    console.log('  \u2713 ' + name);
}

// --- снимки бэкендов ---------------------------------------------------------
// Новый балансер: пометка приходит готовой (GET /api/v1/backends, R304-поля).
function donorBackend(overrides) {
    return Object.assign({
        id: 'cppworker-cyborg-bundled-agent',
        backendType: 'ollama',
        type: 'ollama',
        status: 'healthy',
        host: '192.0.2.11',
        ollamaPort: 11434,
        role: 'donor',
        donor: true,
        canLoadOwnModels: false,
        ownCopies: [],
        donorNote: 'донор мастера cppworker-gpu-bundled-agent: отдаёт VRAM через rpc-server и не держит своих копий',
        donorFor: { backendId: 'cppworker-gpu-bundled-agent', host: '192.0.2.10' },
        ollama: { runningModels: [] },
    }, overrides || {});
}

function mainBackend(overrides) {
    return Object.assign({
        id: 'cppworker-gpu-bundled-agent',
        backendType: 'ollama',
        type: 'ollama',
        status: 'healthy',
        host: '192.0.2.10',
        ollamaPort: 11434,
        role: 'main',
        donor: false,
        canLoadOwnModels: true,
        ownCopies: [],
        ollama: { runningModels: [] },
    }, overrides || {});
}

function renderBackendsPage(backends) {
    byId['backendsManageBody'] = makeEl('backendsManageBody');
    R.backendsPage(backends);
    return byId['backendsManageBody'].innerHTML;
}

function renderModelsPage(backends) {
    byId['modelsGrid'] = makeEl('modelsGrid');
    byId['backendLoadList'] = makeEl('backendLoadList');
    byId['modelsTotal'] = makeEl('modelsTotal');
    R.modelsPage(backends);
    return byId['modelsGrid'].innerHTML;
}

// Модель на ollama-бэкенде нужна, чтобы у карточки вообще была кнопка load
// (для llama_cpp её скрывает Round 18e — там управление на GGUF-странице).
function withModel(backend, name) {
    return Object.assign({}, backend, {
        ollama: { runningModels: [{ name: name, family: 'gemma', parameterSize: '2B', quantization: 'Q4_K_M' }] },
    });
}

console.log('R304: донор держит только память (панель балансера)');

// --- 1. донор с назначенным мастером ----------------------------------------
check('Донор: бейдж «⛓ донор (мастер: X)» рядом со статусом', function () {
    const html = renderBackendsPage([mainBackend(), donorBackend()]);
    assert.ok(html.indexOf('class="badge badge-info"') !== -1, 'нет бейджа badge-info: ' + html.slice(0, 600));
    assert.ok(html.indexOf('донор (мастер: cppworker-gpu-bundled-agent)') !== -1,
        'бейдж обязан называть мастера: ' + html.slice(0, 600));
    assert.ok(html.indexOf('⛓') !== -1, 'у пометки донора свой символ');
});

check('Донор: в раскрытой строке — чей это донор и что он не грузит', function () {
    const html = renderBackendsPage([mainBackend(), donorBackend()]);
    assert.ok(html.indexOf('Донор: только память мастера') !== -1, 'нет секции донора в раскрытой строке');
    assert.ok(html.indexOf('отдаёт VRAM мастеру cppworker-gpu-bundled-agent через rpc-server') !== -1,
        'пояснение обязано называть мастера и rpc-server');
    assert.ok(html.indexOf('свои модели не грузит') !== -1, 'пояснение обязано называть запрет');
});

// --- 2. донор, который вопреки роли держит свою копию ------------------------
check('Донор со своей копией: бейдж «своя копия: 1» и предупреждение', function () {
    const html = renderBackendsPage([mainBackend(), donorBackend({ ownCopies: ['gemma'] })]);
    assert.ok(html.indexOf('своя копия: 1') !== -1, 'нет бейджа-нарушения: ' + html.slice(0, 800));
    assert.ok(html.indexOf('class="badge badge-warning"') !== -1, 'нарушение рисуется badge-warning');
    assert.ok(html.indexOf('title="gemma"') !== -1, 'title бейджа перечисляет имена копий');
    assert.ok(html.indexOf('у донора загружена своя копия: gemma') !== -1,
        'в раскрытой строке — предупреждение с именами');
    assert.ok(html.indexOf('нарушение роли') !== -1, 'и что с этим делать');
});

check('Донор с копией в загрузке: имя с пометкой (loading) тоже видно', function () {
    const html = renderBackendsPage([mainBackend(), donorBackend({ ownCopies: ['gemma (loading)'] })]);
    assert.ok(html.indexOf('своя копия: 1') !== -1, 'грузящаяся копия — тоже нарушение');
    assert.ok(html.indexOf('gemma (loading)') !== -1, 'имена копий попадают в текст');
});

// --- 3. донор без мастера ----------------------------------------------------
check('Донор без donorFor.backendId: «мастер не назначен», без падения', function () {
    const b = donorBackend({ donorFor: undefined });
    let html = '';
    assert.doesNotThrow(function () { html = renderBackendsPage([b]); }, 'донор без мастера не должен ронять отрисовку');
    assert.ok(html.indexOf('донор (мастер не назначен)') !== -1, 'нет честной подписи об отсутствии мастера: ' + html.slice(0, 600));
    assert.ok(html.indexOf('Донор: мастер не назначен') !== -1, 'пояснение в раскрытой строке тоже честное');
});

// --- 4. регресс-контроль: не-донор не меняется ------------------------------
check('Не-донор (main/standalone): бейджа донора нет, «Загрузить» активна', function () {
    const main = withModel(mainBackend(), 'gemma');
    const standalone = withModel(mainBackend({ id: 'cppworker-solo', role: 'standalone' }), 'gemma');
    const pageHtml = renderBackendsPage([main, standalone]);
    assert.strictEqual(pageHtml.indexOf('донор (мастер'), -1, 'у не-донора пометки донора быть не должно');
    assert.strictEqual(pageHtml.indexOf('Донор: '), -1, 'и секции донора в раскрытой строке тоже');

    const grid = renderModelsPage([main]);
    assert.ok(/class="btn btn-load"[^>]*>Загрузить</.test(grid), 'у не-донора кнопка load отрисована');
    assert.strictEqual(/btn btn-load[^>]*disabled/.test(grid), false,
        'у не-донора кнопка load обязана БЫТЬ АКТИВНОЙ: ' + (grid.match(/btn btn-load[^>]*/) || [''])[0]);
});

check('Не-донор: действие load уходит в API (блокировка не про него)', function () {
    BACKENDS.length = 0;
    BACKENDS.push(withModel(mainBackend(), 'gemma'));
    apiCalls.length = 0;
    ui.executeModelOperation(mainBackend().id, 'load', 'gemma');
    assert.strictEqual(apiCalls.length, 1, 'у не-донора запрос ОБЯЗАН уйти в API');
    assert.strictEqual(apiCalls[0].operation, 'load');
});

// --- 5. донор: модель нельзя поднять из карточки ----------------------------
check('Донор: кнопка «Загрузить» на карточке погашена и объясняет причину', function () {
    const donor = withModel(donorBackend(), 'gemma');
    const grid = renderModelsPage([donor]);
    const loadBtn = (grid.match(/<button class="btn btn-load"[^>]*>/) || [''])[0];
    assert.ok(loadBtn.indexOf('disabled') !== -1, 'кнопка load у донора обязана быть disabled: ' + loadBtn);
    assert.ok(loadBtn.indexOf('Донор не поднимает свои копии моделей') !== -1,
        'title кнопки обязан объяснять роль, а не молча гаснуть: ' + loadBtn);
    // «Выгрузить» остаётся рабочей: снять нарушающую копию — законное действие.
    const unloadBtn = (grid.match(/<button class="btn btn-unload"[^>]*>/) || [''])[0];
    assert.ok(unloadBtn && unloadBtn.indexOf('disabled') === -1,
        'unload у донора НЕ блокируется (выгрузить копию — как раз то, что нужно): ' + unloadBtn);
});

// --- 6. старый балансер: полей R304 нет, есть только role --------------------
check('Старый балансер (только role:donor): пометка всё равно есть', function () {
    const legacyDonor = {
        id: 'legacy-donor', backendType: 'llama_cpp', type: 'llama_cpp',
        status: 'healthy', host: '192.0.2.12', cppWorkerPort: 18092, role: 'donor',
        llamaCpp: { loadedModels: [] },
    };
    const legacyMain = {
        id: 'legacy-main', backendType: 'llama_cpp', type: 'llama_cpp',
        status: 'healthy', host: '192.0.2.10', cppWorkerPort: 18091, role: 'main',
        llamaCpp: { loadedModels: [] },
    };
    const html = renderBackendsPage([legacyMain, legacyDonor]);
    assert.ok(html.indexOf('донор (мастер: legacy-main)') !== -1,
        'fallback обязан найти мастера по role==="main": ' + html.slice(0, 800));
});

check('Старый балансер: донор без мастера в списке — «мастер не назначен»', function () {
    // Полный список панели тоже без главного: иначе fallback честно нашёл бы
    // мастера в App.context.data (он ищет по ВСЕМУ загруженному списку).
    BACKENDS.length = 0;
    const legacyDonor = {
        id: 'legacy-donor', backendType: 'llama_cpp', type: 'llama_cpp',
        status: 'healthy', host: '192.0.2.12', cppWorkerPort: 18092, role: 'donor',
        llamaCpp: { loadedModels: [] },
    };
    const html = renderBackendsPage([legacyDonor]);
    assert.ok(html.indexOf('донор (мастер не назначен)') !== -1, 'без role==="main" мастера нет: ' + html.slice(0, 600));
});

// --- 7. отказ на уровне действия (последний уровень перед API) --------------
check('executeModelOperation("load") у донора: запрос в API НЕ ушёл', function () {
    BACKENDS.length = 0;
    BACKENDS.push(withModel(donorBackend(), 'gemma'));
    apiCalls.length = 0;
    ui.executeModelOperation(withModel(donorBackend(), 'gemma').id, 'load', 'gemma');
    assert.strictEqual(apiCalls.length, 0, 'load у донора обязан отказывать ДО запроса: ' + JSON.stringify(apiCalls));
});

check('executeModelOperation("pull") у донора: запрос в API НЕ ушёл', function () {
    BACKENDS.length = 0;
    BACKENDS.push(withModel(donorBackend(), 'gemma'));
    apiCalls.length = 0;
    ui.executeModelOperation(donorBackend().id, 'pull', 'gemma:2b');
    assert.strictEqual(apiCalls.length, 0, 'pull у донора обязан отказывать ДО запроса: ' + JSON.stringify(apiCalls));
});

check('executeModelOperation("unload") у донора: НЕ блокируется', function () {
    BACKENDS.length = 0;
    BACKENDS.push(withModel(donorBackend(), 'gemma'));
    apiCalls.length = 0;
    ui.executeModelOperation(donorBackend().id, 'unload', 'gemma');
    assert.strictEqual(apiCalls.length, 1, 'выгрузка нарушающей копии — законное действие, её не блокируем');
    assert.strictEqual(apiCalls[0].operation, 'unload');
});

// --- 8. чип донора в панели раскладки называет мастера ----------------------
check('LayoutPanel: чип донора — «🔗 донор (мастер: X)», а не просто «узел»', function () {
    // Панель раскладки живёт отдельным модулем (js/monitor/layout-panel.js) со
    // своими MonitorApp/esc — подгружаем её в этом же процессе с минимальными
    // заглушками, чтобы проверить именно текст чипа.
    global.MonitorApp = global.MonitorApp || {};
    global.MonitorApp.esc = function (s) { return String(s === null || s === undefined ? '' : s); };
    global.MonitorApp.T = tr;
    global.MonitorApiXhr = {
        fetchBackends: function () { return Promise.resolve({ backends: [] }); },
        setBackendRole: function () { return Promise.resolve(true); },
    };
    require('../monitor/layout-panel.js');
    const ML = global.MonitorLayout;
    assert.ok(ML && typeof ML.rowLayoutCell === 'function', 'MonitorLayout должен быть опубликован');
    ML._state.backends = [
        { id: 'cppworker-gpu-bundled-agent', backendType: 'llama_cpp', role: 'main' },
        { id: 'cppworker-cyborg-bundled-agent', backendType: 'llama_cpp', role: 'donor' },
    ];
    const cell = ML.rowLayoutCell(ML._state.backends[1]);
    assert.ok(cell.indexOf('донор (мастер: cppworker-gpu-bundled-agent)') !== -1,
        'чип донора обязан называть мастера: ' + cell);
});

console.log('\ndonor-mark-r304: ' + passed + ' проверок пройдено');
