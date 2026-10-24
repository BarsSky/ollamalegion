// setup-plan-dom.test.js — R103 (S5 плана 2026-10-07): шаг мастера «состав
// кластера» с мок-DOM и мок-fetch (без браузера).
//
// Запуск: node webui/js/modules/setup-plan-dom.test.js
//
// Что проверяем:
//   1. Шаг ПОДКЛЮЧЁН к мастеру: setup-wizard.js монтирует SetupPlan в
//      #setupPlanPanel на шаге 3, контейнер появляется в разметке шага, а сам
//      модуль подключён тегом script (иначе шаг «состав кластера» останется
//      пустым, и это не видно ни в одном чистом тесте).
//   2. Все i18n-ключи есть в ru.js и en.js (в т.ч. подпись шага, которую мастер
//      берёт через I18N.t).
//   3. mount(): POST /api/v1/placement/plan и таблица с ЧИСЛАМИ (нужно / максимум
//      на узле / сумма, трафик обеих стратегий).
//   4. Пустой стенд: честная подсказка, а не пустая таблица.
//   5. apply(): GET текущего состояния → PUT, причём правила и группы, которых
//      план не касается, переносятся (PUT заменяет секции целиком).
//   6. Честность: persisted=false и отказ сервера показываются как есть.

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

let passed = 0;
const failures = [];
function check(name, fn) {
    try { fn(); passed++; console.log('  \u2713 ' + name); }
    catch (e) { failures.push(name + ': ' + (e && e.message)); console.error('  \u2717 ' + name + ' - ' + (e && e.message)); }
}
async function checkAsync(name, fn) {
    try { await fn(); passed++; console.log('  \u2713 ' + name); }
    catch (e) { failures.push(name + ': ' + (e && e.message)); console.error('  \u2717 ' + name + ' - ' + (e && e.message)); }
}

// --- mocks -----------------------------------------------------------------

global.window = global;
global.document = {
    readyState: 'loading',
    addEventListener: function () {},
    getElementById: function (id) { return id === 'setupPlanPanel' ? panel : null; },
    querySelectorAll: function () { return []; },
};
const panel = { innerHTML: '' };
const store = {};
global.localStorage = {
    getItem: function (k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem: function (k, v) { store[k] = String(v); },
    removeItem: function (k) { delete store[k]; },
};

require('../i18n/ru.js');
require('../i18n/en.js');
const RU = global.I18N_RU;
const EN = global.I18N_EN;

global.I18N = {
    getLang: function () { return 'ru'; },
    t: function (key) {
        if (RU[key] === undefined) { return key; }
        return RU[key];
    },
};
global.Api = { getAuthHeaders: function () { return { 'Content-Type': 'application/json' }; } };
global.WEBUI_CONFIG = { API_BASE: 'http://balancer.test:18081' };

const calls = [];
let routes = {};
global.fetch = function (url, opts) {
    const method = (opts && opts.method) || 'GET';
    const body = opts && opts.body ? JSON.parse(opts.body) : null;
    calls.push({ method: method, url: url, body: body });
    const key = method + ' ' + url.replace('http://balancer.test:18081', '');
    const handler = routes[key];
    if (!handler) {
        return Promise.resolve({ ok: false, status: 404, text: function () { return Promise.resolve('{"error":"no route"}'); } });
    }
    const res = typeof handler === 'function' ? handler(body) : handler;
    return Promise.resolve({
        ok: res.status >= 200 && res.status < 300,
        status: res.status,
        text: function () { return Promise.resolve(JSON.stringify(res.body)); },
    });
};

const MODULE_SRC = fs.readFileSync(path.join(__dirname, 'setup-plan.js'), 'utf8');
const WIZARD_SRC = fs.readFileSync(path.join(__dirname, 'setup-wizard.js'), 'utf8');
const INDEX_HTML = fs.readFileSync(path.join(__dirname, '..', '..', 'index.html'), 'utf8');

require('./setup-wizard-presets.js');
require('./setup-plan.js');
const SP = global.SetupPlan;

// --- 1. подключение к мастеру ---------------------------------------------

check('мастер монтирует SetupPlan в #setupPlanPanel', function () {
    assert.ok(WIZARD_SRC.indexOf('SetupPlan.mount') >= 0,
        'без вызова mount шаг останется пустым');
    assert.ok(WIZARD_SRC.indexOf('setupPlanPanel') >= 0,
        'контейнер шага должен быть в разметке мастера');
});

check('шаг 2 мастера — «состав кластера», а не выбор режима (R195: шаг типа бэкенда удалён)', function () {
    assert.ok(WIZARD_SRC.indexOf('case 2: html = renderClusterPlanStep()') >= 0,
        'шаг 2 должен рисовать состав кластера');
    assert.ok(WIZARD_SRC.indexOf("window.SettingsUI.setupModeSelector()") < 0,
        'переключатель режимов на шаге 3 больше не вызывается');
});

check('модуль подключён тегом script в index.html', function () {
    assert.ok(/js\/modules\/setup-plan\.js\?v=/.test(INDEX_HTML), 'модуль должен быть подключён');
});

check('i18n: подпись шага мастера есть в обоих языках', function () {
    assert.ok(RU['wizard.step_cluster'] && EN['wizard.step_cluster']);
    assert.ok(RU['plan.stepHint'] && EN['plan.stepHint']);
});

// --- 2. i18n-ключи модуля -------------------------------------------------

check('все i18n-ключи модуля и шага есть в ru.js и en.js', function () {
    const keys = new Set();
    const collect = function (src) {
        const re = /T\('([^']+)'/g;
        let m;
        while ((m = re.exec(src)) !== null) { keys.add(m[1]); }
    };
    collect(MODULE_SRC);
    collect(WIZARD_SRC.match(/renderClusterPlanStep[\s\S]{0,600}/)[0]);
    ['single', 'replicated', 'sharded', 'pool', 'auto', 'rpc', 'layer', 'row'].forEach(function (s) {
        keys.add('plan.strategy.' + s);
    });
    const missingRU = [];
    const missingEN = [];
    keys.forEach(function (k) {
        if (RU[k] === undefined) { missingRU.push(k); }
        if (EN[k] === undefined) { missingEN.push(k); }
    });
    assert.deepStrictEqual(missingRU, [], 'нет переводов ru: ' + missingRU.join(', '));
    assert.deepStrictEqual(missingEN, [], 'нет переводов en: ' + missingEN.join(', '));
});

// --- 3. план с числами ----------------------------------------------------

const planResponse = {
    status: 200,
    body: {
        success: true, dryRun: true, lang: 'ru', discoveredModels: 2,
        plans: [
            {
                model: 'qwen3-14b', strategy: 'sharded', source: 'proposal',
                reason: 'ни один узел не вмещает модель, а сумма по 2 узлам — вмещает',
                sizeGB: 8.0, needGB: 10.0, maxNodeGB: 6.0, sumFreeGB: 12.0,
                knownSize: true, degraded: false,
                traffic: {
                    layer: { known: true, tokenOverheadPercent: 0.22, prefillMs: 37.4 },
                    row: { known: true, tokenOverheadPercent: 10.5, prefillMs: 1198 },
                },
                issues: [{ check: 'rpc_unreachable', level: 'error', text: 'узел node-b не отвечает на 192.0.2.11:50052' }],
            },
            {
                model: 'gemma-3-4b', strategy: 'replicated', source: 'proposal',
                reason: 'модель влезает в 2 узла по отдельности',
                sizeGB: 3.0, needGB: 3.8, maxNodeGB: 6.0, sumFreeGB: 12.0,
                knownSize: true, degraded: false, traffic: null, issues: [],
            },
        ],
        placement: {
            enabled: true, fallback: 'error',
            models: [
                { model: 'qwen3-14b', strategy: 'auto', auto: { prefer: ['sharded', 'single'] } },
                { model: 'gemma-3-4b', strategy: 'auto', auto: { prefer: ['replicated', 'single'] } },
            ],
        },
        backendGroups: [{ id: 'group-qwen3-14b', model: 'qwen3-14b', strategy: 'layer', members: [] }],
        warnings: [],
    },
};

(async function run() {
    await checkAsync('mount(): POST /plan и таблица с числами', async function () {
        routes = { 'POST /api/v1/placement/plan': planResponse };
        calls.length = 0;
        SP.reset();
        await SP.mount('setupPlanPanel');

        assert.strictEqual(calls.length, 1, 'ожидался один запрос плана');
        assert.strictEqual(calls[0].url, 'http://balancer.test:18081/api/v1/placement/plan');
        assert.strictEqual(calls[0].method, 'POST');

        assert.ok(panel.innerHTML.indexOf('qwen3-14b') >= 0, 'модель должна быть в таблице');
        assert.ok(panel.innerHTML.indexOf('gemma-3-4b') >= 0, 'вторая модель тоже');
        assert.ok(panel.innerHTML.indexOf('10.0') >= 0, 'нужно по VRAM (число)');
        assert.ok(panel.innerHTML.indexOf('6.0') >= 0, 'максимум на узле (число)');
        assert.ok(panel.innerHTML.indexOf('12.0') >= 0, 'сумма по узлам (число)');
        assert.ok(panel.innerHTML.indexOf('10.5') >= 0, 'процент row-раскладки');
        assert.ok(panel.innerHTML.indexOf('1.2') >= 0, 'секунды на префилл 512');
        assert.ok(panel.innerHTML.indexOf('sharded (слоями)') >= 0, 'стратегия переведена');
        assert.ok(panel.innerHTML.indexOf('не отвечает на 192.0.2.11:50052') >= 0,
            'замечание §7 с адресом узла должно доехать до шага');
        assert.ok(panel.innerHTML.indexOf('Найдено моделей') >= 0, 'сколько моделей найдено');
    });

    await checkAsync('пустой стенд: честная подсказка', async function () {
        routes = {
            'POST /api/v1/placement/plan': {
                status: 200,
                body: { success: true, discoveredModels: 0, plans: [], placement: { enabled: true, models: [] }, backendGroups: [], warnings: [] },
            },
        };
        SP.reset();
        await SP.mount('setupPlanPanel');
        assert.ok(panel.innerHTML.indexOf('Модели не найдены') >= 0,
            'пустой план должен объяснять, что делать, а не показывать пустую таблицу');
    });

    await checkAsync('ошибка плана показывается, а не проглатывается', async function () {
        routes = {};
        SP.reset();
        let failed = false;
        try { await SP.mount('setupPlanPanel'); } catch (e) { failed = true; }
        assert.ok(failed, 'ошибка должна быть выброшена вызывающему');
        assert.ok(panel.innerHTML.indexOf('Не удалось построить план') >= 0);
    });

    // --- 5. запись политики ----------------------------------------------

    const currentState = {
        status: 200,
        body: {
            success: true,
            placement: {
                enabled: true, fallback: 'error',
                models: [
                    { model: 'чужая-модель', strategy: 'single', reason: 'правило оператора' },
                    { model: 'qwen3-14b', strategy: 'auto' },
                ],
                classes: [{ match: { sizeGB: '>20' }, strategy: 'single', reason: 'крупные — на один узел' }],
            },
            backendGroups: [
                { id: 'group-чужая', model: 'чужая-модель', strategy: 'layer', members: [] },
                { id: 'group-qwen3-14b', model: 'qwen3-14b', strategy: 'row', members: [] },
            ],
        },
    };

    await checkAsync('apply(): GET → PUT, чужие правила и группы сохранены', async function () {
        routes = {
            'POST /api/v1/placement/plan': planResponse,
            'GET /api/v1/placement': currentState,
            'PUT /api/v1/placement': { status: 200, body: { success: true, applied: true, persisted: true } },
        };
        SP.reset();
        await SP.mount('setupPlanPanel');
        calls.length = 0;
        const res = await SP.apply();

        assert.strictEqual(res.applied, true);
        assert.strictEqual(calls.length, 2, 'запись = чтение текущего состояния + PUT');
        const put = calls[1];
        assert.strictEqual(put.method, 'PUT');

        const models = put.body.placement.models.map(function (r) { return r.model; });
        assert.ok(models.indexOf('чужая-модель') >= 0, 'чужое правило обязано сохраниться');
        assert.strictEqual(models.filter(function (m) { return m === 'qwen3-14b'; }).length, 1,
            'правило планируемой модели должно быть заменено, а не задублировано');
        assert.strictEqual(models.filter(function (m) { return m === 'gemma-3-4b'; }).length, 1,
            'вторая планируемая модель тоже должна быть записана');
        assert.strictEqual(put.body.placement.classes.length, 1,
            'правила классов оператора не должны потеряться');

        const groups = put.body.backendGroups.map(function (g) { return g.id; });
        assert.ok(groups.indexOf('group-чужая') >= 0, 'чужая группа обязана сохраниться');
        assert.strictEqual(groups.filter(function (g) { return g === 'group-qwen3-14b'; }).length, 1,
            'группа планируемой модели заменяется, а не дублируется');
        assert.strictEqual(put.body.placement.enabled, true);

        assert.ok(panel.innerHTML.indexOf('Политика применена') >= 0, 'результат должен быть виден');
        assert.ok(panel.innerHTML.indexOf('Конфиг сохранён на диск') >= 0);
    });

    await checkAsync('apply(): persisted=false показывается честно', async function () {
        routes = {
            'POST /api/v1/placement/plan': planResponse,
            'GET /api/v1/placement': currentState,
            'PUT /api/v1/placement': {
                status: 200,
                body: { success: true, applied: true, persisted: false, persistError: 'read-only file system' },
            },
        };
        SP.reset();
        await SP.mount('setupPlanPanel');
        await SP.apply();
        assert.ok(panel.innerHTML.indexOf('НЕ сохранён') >= 0, 'нельзя показывать успех записи файла');
        assert.ok(panel.innerHTML.indexOf('read-only') >= 0, 'причина должна быть видна');
    });

    await checkAsync('apply(): отказ сервера виден оператору', async function () {
        routes = {
            'POST /api/v1/placement/plan': planResponse,
            'GET /api/v1/placement': currentState,
            'PUT /api/v1/placement': {
                status: 400,
                body: { success: false, error: 'политика не применена: конфиг не проходит валидацию', warnings: ['backendGroups[0]: у воркера обязателен rpcEndpoint'] },
            },
        };
        SP.reset();
        await SP.mount('setupPlanPanel');
        let failed = false;
        try { await SP.apply(); } catch (e) { failed = true; }
        assert.ok(failed, 'ошибка должна дойти до вызывающего');
        assert.ok(panel.innerHTML.indexOf('Записать политику не удалось') >= 0);
    });

    // --- чистые функции ---------------------------------------------------

    check('buildApplyPayload: план без моделей не даёт записывать пустоту', function () {
        assert.strictEqual(SP.buildApplyPayload(null, null), null);
        const payload = SP.buildApplyPayload({ placement: { enabled: true, models: [] }, plans: [], backendGroups: [] }, null);
        assert.ok(payload, 'план с пустым списком моделей всё равно даёт валидное тело');
        assert.deepStrictEqual(payload.placement.models, []);
    });

    check('trafficPercent: неизвестные числа не выдумываются', function () {
        assert.strictEqual(SP.trafficPercent(null, 'layer'), '');
        assert.strictEqual(SP.trafficPercent({ layer: { known: false } }, 'layer'), '');
        assert.strictEqual(SP.trafficPercent({ layer: { known: true, tokenOverheadPercent: -1, prefillMs: 0 } }, 'layer'), '? / 0.0 s');
    });

    // --- чистая функция сопоставления с пресетами ------------------------

    check('matchesOf: подсвечиваются раскладки, которые есть в плане', function () {
        // В плане ДВЕ модели: 14B раскладывается слоями, 4B реплицируется — и
        // подсветка обязана показать оба случая (план на несколько моделей может
        // соответствовать нескольким пресетам сразу).
        assert.deepStrictEqual(SP.matchesOf(planResponse.body), ['replicated', 'sharded-layer']);

        // Если группа предлагает row, подсветка переключается на неё.
        const rowPlan = {
            plans: [{
                model: 'm', strategy: 'sharded',
                backendGroup: { id: 'g', model: 'm', strategy: 'row', members: [] },
            }],
        };
        assert.deepStrictEqual(SP.matchesOf(rowPlan), ['sharded-row']);

        // Одиночная модель — single.
        assert.deepStrictEqual(SP.matchesOf({ plans: [{ model: 'm', strategy: 'single' }] }), ['single']);

        // Пустой план — подсвечивать нечего.
        assert.deepStrictEqual(SP.matchesOf(null), []);
        assert.deepStrictEqual(SP.matchesOf({ plans: [] }), []);
    });

    check('справка по раскладкам отрисована с подсветкой', function () {
        assert.ok(panel.innerHTML.indexOf('Какие раскладки бывают') >= 0,
            'оператору нужен разбор того, что означает предложенная раскладка');
        assert.ok(panel.innerHTML.indexOf('sp-preset-active') >= 0, 'совпавший пресет должен быть подсвечен');
        assert.ok(panel.innerHTML.indexOf('подходит вашему стенду') >= 0, 'и подписан по-человечески');
        assert.ok(panel.innerHTML.indexOf('Две машины, слоями') >= 0, 'текст пресета переведён');
    });

    check('чистовая уборка R107: мастер без выбора режима', function () {
    // Режим работы и его параметры убраны из мастера целиком: пять шагов, никаких
// карточек/радио режима, шаг 3 — общие параметры (R195: шаг типа бэкенда удалён), а payload не пишет
    // operatingMode (PUT применяет поле только если оно непустое, значит текущее
    // значение на стенде сохраняется).
    assert.ok(/var totalSteps = 4;/.test(WIZARD_SRC), 'R195: шагов должно стать четыре (шаг типа бэкенда удалён)');
    assert.ok(WIZARD_SRC.indexOf('renderModeParamsStep') < 0, 'шаг «параметры режима» удалён');
    assert.ok(WIZARD_SRC.indexOf('renderModeSelectionStep') < 0, 'мёртвый рендер выбора режима удалён');
    assert.ok(WIZARD_SRC.indexOf('validateWizardModeParams') < 0, 'валидатор режима удалён');
    assert.ok(WIZARD_SRC.indexOf('name="operatingMode"') < 0, 'радио режима мастер больше не рисует');
    assert.ok(WIZARD_SRC.indexOf('case 3: html = renderGeneralSettingsStep()') >= 0,
        'шаг 3 — общие параметры');
    assert.ok(WIZARD_SRC.indexOf('case 4: html = renderSummaryStep()') >= 0, 'шаг 4 — сводка');
    assert.ok(WIZARD_SRC.indexOf('operatingMode: wizardState.operatingMode') < 0,
        'payload мастера не должен писать operatingMode');
    assert.ok(WIZARD_SRC.indexOf('wizard.summary_placement') >= 0,
        'сводка должна говорить про политику размещения, а не про режим');
});

console.log('\n' + passed + ' passed, ' + failures.length + ' failed');
    if (failures.length) {
        failures.forEach(function (f) { console.error('  - ' + f); });
        process.exit(1);
    }
})();
