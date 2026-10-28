// backend-merge-dom.test.js — R102 (S6 плана 2026-10-07): окно «Объединение
// бэкендов» с мок-DOM и мок-fetch (без браузера).
//
// Запуск: node webui/js/modules/backend-merge-dom.test.js
//
// Что проверяем (то, что не поймать чистыми функциями):
//   1. Модуль ПОДКЛЮЧЁН: контейнер #backendMergePanel есть в index.html, тег
//      script с модулем тоже (иначе окно просто не появится на странице).
//   2. Все i18n-ключи модуля переведены И в ru.js, И в en.js: окно обязано
//      работать в обоих языках, а не показывать сырые ключи.
//   3. mount(): GET списка бэкендов и отрисовка узлов в контейнер.
//   4. Сборка композиции: мастер/воркеры, RPC-эндпоинт воркера, отказ при
//      менее чем двух узлах.
//   5. Проверка (dry-run): ровно два запроса — план (числа трафика) и валидатор
//      (§7), и то, что замечания с числами доезжают до панели.
//   6. Применение: GET текущего состояния → PUT, причём ЧУЖИЕ правила и группы
//      переносятся (PUT заменяет секции целиком).
//   7. Честность persisted=false: «действует, но не сохранено» вместо тихого успеха.
//   8. «Разъединить»: снимается и группа, И правило её модели (иначе 503 на
//      каждый запрос после разъединения), и об этом сказано.

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
async function checkAsync(name, fn) {
    try {
        await fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' - ' + (e && e.message));
    }
}

// --- mocks -----------------------------------------------------------------

global.window = global;
const domListeners = {};
global.document = {
    readyState: 'loading',
    addEventListener: function (name, fn) { domListeners[name] = fn; },
    getElementById: function (id) {
        if (id === 'backendMergePanel') { return panel; }
        return null;
    },
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
assert.ok(RU && EN, 'ru.js и en.js должны быть загружены');

const missingKeys = [];
let currentLang = 'ru';
global.I18N = {
    getLang: function () { return currentLang; },
    t: function (key, vars) {
        const pack = currentLang === 'ru' ? RU : EN;
        let s = pack[key];
        if (s === undefined) {
            if (missingKeys.indexOf(key) === -1) { missingKeys.push(key); }
            s = key;
        }
        if (vars) {
            Object.keys(vars).forEach(function (p) { s = String(s).replace('{' + p + '}', vars[p]); });
        }
        return s;
    },
};

let confirmAnswer = true;
const confirmMessages = [];
global.confirm = function (message) {
    confirmMessages.push(message);
    return confirmAnswer;
};
global.Api = { getAuthHeaders: function () { return { 'Content-Type': 'application/json', 'X-API-Token': 't' }; } };
global.WEBUI_CONFIG = { API_BASE: 'http://balancer.test:18081', API_TOKEN: 't' };

// fetch: маршрутизация по методу и пути, с записью вызовов.
const calls = [];
let routes = {};
function mockFetch(url, opts) {
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
}
global.fetch = mockFetch;

// --- inventory: модуль / index.html ----------------------------------------

const MODULE_SRC = fs.readFileSync(path.join(__dirname, 'backend-merge.js'), 'utf8');
const INDEX_HTML = fs.readFileSync(path.join(__dirname, '..', '..', 'index.html'), 'utf8');

// R238e: модуль подписывается на i18n:changed, чтобы перерисовываться при смене
// языка. Перехватываем window.addEventListener до require модуля.
const winListeners = {};
global.addEventListener = function (name, fn) { winListeners[name] = fn; };

require('./backend-merge.js');
const BM = global.BackendMerge;

// --- 1. подключение модуля ------------------------------------------------

check('контейнер #backendMergePanel объявлен в index.html', function () {
    assert.ok(INDEX_HTML.indexOf('id="backendMergePanel"') >= 0,
        'без контейнера окно некуда рисовать');
});

check('тег script с модулем есть в index.html', function () {
    assert.ok(/js\/modules\/backend-merge\.js\?v=/.test(INDEX_HTML),
        'модуль должен быть подключён (иначе окно не появится)');
});

check('контейнер находится на странице «Бэкенды»', function () {
    const pageAt = INDEX_HTML.indexOf('id="backends-page"');
    const panelAt = INDEX_HTML.indexOf('id="backendMergePanel"');
    assert.ok(pageAt >= 0 && panelAt > pageAt, 'панель должна быть внутри страницы бэкендов');
});

check('i18n: ключ заголовка объявлен в обоих языках', function () {
    assert.ok(RU['merge.title'] && EN['merge.title'], 'merge.title должен быть переведён');
});

// --- 1b. ОФОРМЛЕНИЕ (R175) ------------------------------------------------
//
// Живая жалоба оператора: «css стили не применились к новой вкладке». Блок
// «Объединение бэкендов» рисовался голыми контролами: разметка использовала
// ТОЛЬКО собственные классы .bm-*, ни одного правила для них в стилях не было,
// а штатные примитивы приложения (form-control/btn/form-group) не применялись.
// Эти проверки не дают дефекту вернуться молча.

const CSS_SRC = fs.readFileSync(path.join(__dirname, '..', '..', 'css', 'components.css'), 'utf8');

check('контролы панели используют штатные примитивы (form-control)', function () {
    const inputs = (MODULE_SRC.match(/class=\\?"form-control\\?"/g) || []).length;
    assert.ok(inputs >= 3, 'ожидались минимум 3 поля с классом form-control ' +
        '(модель, канал, стратегия), найдено ' + inputs);
});

check('кнопки панели используют штатные классы btn (проверка/применение)', function () {
    assert.ok(/class=\\?"btn btn-secondary\\?"/.test(MODULE_SRC),
        'кнопкам «Проверить»/«Замерить» нужен класс btn btn-secondary');
    assert.ok(/class=\\?"btn btn-primary\\?"/.test(MODULE_SRC),
        'кнопке «Применить» нужен класс btn btn-primary');
});

check('у панели есть правила в components.css (а не только .bm-* классы)', function () {
    assert.ok(/#backendMergePanel/.test(CSS_SRC),
        'ни одного правила для #backendMergePanel — блок снова будет голым');
    ['bm-field', 'bm-actions', 'bm-backends'].forEach(function (cls) {
        assert.ok(CSS_SRC.indexOf('.' + cls) >= 0, 'в стилях нет правила для .' + cls);
    });
});

check('список узлов — таблица С ЗАГОЛОВКАМИ', function () {
    assert.ok(/<thead>/.test(MODULE_SRC), 'у таблицы узлов должна быть шапка');
    ['merge.col.node', 'merge.col.host', 'merge.col.type', 'merge.col.master'].forEach(function (k) {
        assert.ok(MODULE_SRC.indexOf(k) >= 0, 'нет заголовка столбца ' + k);
    });
});

// --- 1c. R240: выбор модели из кластера и подпись над полем ----------------

const POLISH_CSS = fs.readFileSync(path.join(__dirname, '..', '..', 'css', 'polish.css'), 'utf8');

check('R240: модели узла читаются из llamaCpp.loadedModels и ollama.availableModels', function () {
    const cpp = BM.modelsOfBackend({
        llamaCpp: { loadedModels: [{ name: 'qwen3:14b' }, { name: 'gemma3:4b' }, { name: 'qwen3:14b' }] },
    });
    assert.deepStrictEqual(cpp, ['qwen3:14b', 'gemma3:4b'], 'дубликаты не нужны, порядок сохраняем');
    const avail = BM.modelsOfBackend({
        llamaCpp: {
            loadedModels: [{ name: 'loaded:1' }],
            availableModels: [{ name: 'on-disk:2' }],
        },
    });
    assert.deepStrictEqual(avail, ['loaded:1', 'on-disk:2'],
        'инвентарь узла тоже должен попадать в подсказку');
    const ollama = BM.modelsOfBackend({
        ollama: { backendCapacity: { availableModels: [{ name: 'llama3:8b' }] } },
    });
    assert.deepStrictEqual(ollama, ['llama3:8b']);
    assert.deepStrictEqual(BM.modelsOfBackend(null), []);
});

check('R240: collectModels сортирует по числу узлов, затем по имени', function () {
    const models = BM.collectModels([
        { id: 'a', models: ['only-a', 'shared'] },
        { id: 'b', models: ['shared', 'second-b'] },
        { id: 'c', models: ['shared'] },
    ]);
    assert.deepStrictEqual(models.map(function (m) { return m.name; }), ['shared', 'only-a', 'second-b']);
    assert.strictEqual(models[0].backends.length, 3, 'у shared должно быть 3 узла');
});

check('R240: modelCoverage считает узлы, где модель реально есть', function () {
    const st = {
        backends: [{ id: 'a', models: ['m1'] }, { id: 'b', models: ['m2'] }],
        selected: { a: true, b: true },
    };
    const cov = BM.modelCoverage(st, 'm1');
    assert.deepStrictEqual(cov.have, ['a']);
    assert.deepStrictEqual(cov.missing, ['b']);
    assert.strictEqual(cov.total, 2);
    // Ничего не выбрано - считаем по всем узлам панели.
    const all = BM.modelCoverage({ backends: st.backends, selected: {} }, 'm1');
    assert.deepStrictEqual(all.have, ['a']);
    assert.strictEqual(all.total, 2);
});

check('R240: подсказка покрытия различает все/часть/ни одного узла', function () {
    const st = {
        backends: [{ id: 'a', models: ['m1'] }, { id: 'b', models: ['m1'] }],
        selected: { a: true, b: true },
    };
    assert.ok(RU['merge.modelCoverageAll'] && EN['merge.modelCoverageAll']);
    assert.ok(BM.modelCoverageText(st, 'm1').indexOf('2') >= 0, 'полное покрытие содержит число узлов');
    st.selected = { a: true };
    assert.strictEqual(BM.modelCoverageText(st, 'm1'), RU['merge.modelCoverageAll'].replace('{total}', '1'));
    st.selected = { a: true, b: true };
    assert.strictEqual(BM.modelCoverageText(st, 'm9'), RU['merge.modelCoverageNone']);
    assert.strictEqual(BM.modelCoverageText(st, ''), '', 'без модели подсказки нет');
});

check('R240: панель содержит datalist с моделями и подсказку покрытия', function () {
    const st = {
        backends: [{ id: 'a', models: ['qwen3:14b'] }, { id: 'b', models: ['qwen3:14b'] }],
        selected: { a: true, b: true },
        masterId: '', strategy: 'layer', model: 'qwen3:14b',
        errorKey: '', result: null, plan: null, report: null,
    };
    const html = BM.panelHtml(st);
    assert.ok(html.indexOf('list="bmModelList"') >= 0, 'у поля модели должен быть datalist');
    assert.ok(html.indexOf('<datalist id="bmModelList"') >= 0, 'datalist не отрисован');
    assert.ok(html.indexOf('value="qwen3:14b"') >= 0, 'модель кластера должна попасть в варианты');
    assert.ok(html.indexOf('bm-model-hint ok') >= 0, 'на полном покрытии подсказка должна быть зелёной');
});

check('R240: подпись поля - отдельный label над контролом (CSS)', function () {
    assert.ok(MODULE_SRC.indexOf('bm-field-label') >= 0, 'в разметке нужен .bm-field-label');
    assert.ok(/#backendMergePanel \.bm-field-label\s*\{[^}]*display:\s*block/.test(POLISH_CSS),
        'label должен быть блочным, иначе он стоит в одной строке с полем');
    assert.ok(/#backendMergePanel \.bm-field > \.form-control[^{]*\{[^}]*display:\s*block/.test(POLISH_CSS),
        'поле должно быть блочным: label сверху, а не впритык слева');
});

// --- 2. i18n-ключи модуля -------------------------------------------------

check('все i18n-ключи модуля есть в ru.js и en.js', function () {
    const re = /T\('([^']+)'/g;
    const keys = new Set();
    let m;
    while ((m = re.exec(MODULE_SRC)) !== null) { keys.add(m[1]); }
    // Динамические ключи: уровень и идентификатор проверки.
    ['error', 'warning', 'hint'].forEach(function (l) { keys.add('merge.level.' + l); });
    ['node_unreachable', 'rpc_unreachable', 'version_mismatch', 'version_unknown',
        'vram_sum_insufficient', 'kv_ram_insufficient', 'kv_unknown', 'traffic_row_high',
        'fits_single', 'backend_type_mismatch', 'homogeneous', 'config'].forEach(function (c) {
        keys.add('merge.check.' + c);
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

// --- 3. mount -------------------------------------------------------------

const backendList = {
    status: 200,
    body: {
        backends: [
            { id: 'node-a', host: '192.0.2.10', type: 'llama_cpp', status: 'healthy' },
            { id: 'node-b', host: '192.0.2.11', type: 'llama_cpp', status: 'healthy' },
            { id: 'img-1', host: '192.0.2.12', type: 'image_cpp', status: 'healthy' },
        ],
    },
};

(async function run() {
    routes = {
        'GET /api/v1/backends': backendList,
        // R240: инвентарь моделей по каждому узлу (для datalist).
        'GET /api/v1/backends/node-a/models': { status: 200, body: { models: [{ name: 'qwen3:14b' }, { name: 'gemma3:4b' }] } },
        'GET /api/v1/backends/node-b/models': { status: 200, body: { models: [{ name: 'qwen3:14b' }] } },
    };
    calls.length = 0;
    BM.reset();

    await checkAsync('mount(): GET /api/v1/backends и отрисовка узлов', async function () {
        const ok = await BM.mount('backendMergePanel');
        assert.strictEqual(ok, true, 'mount должен найти контейнер');
        // R249: к тем же запросам добавился GET /api/v1/placement (нужен фильтру\n        // уже назначенных узлов), поэтому ожидаем 4 вызова.\n        assert.strictEqual(calls.length, 4, 'список бэкендов + раскладка + инвентарь моделей по двум llama_cpp-узлам');
        assert.ok(panel.innerHTML.indexOf('node-a') >= 0, 'узел node-a должен быть в панели');
        assert.ok(panel.innerHTML.indexOf('node-b') >= 0, 'узел node-b должен быть в панели');
        assert.ok(panel.innerHTML.indexOf('img-1') < 0, 'image-бэкенд в раскладку не предлагается');
        // R240: инвентарь узлов догружается асинхронно (рендер не ждёт медленный
        // узел), поэтому дожидаемся его и перерисовываем панель. В живом DOM
        // варианты обновляются точечно в #bmModelList; мок-DOM умеет только
        // полный render().
        await BM.state.modelsReady;
        const names = BM.state.backends.reduce(function (acc, b) {
            return acc.concat(b.models || []);
        }, []);
        assert.ok(names.indexOf('qwen3:14b') >= 0, 'модель node-a должна попасть в состояние');
        assert.ok(names.indexOf('gemma3:4b') >= 0, 'модель node-a (вторая) должна попасть в состояние');
        BM.render('backendMergePanel');
        const html = panel.innerHTML;
        assert.ok(html.indexOf('value="qwen3:14b"') >= 0,
            'модели с диска узла должны предлагаться в поле «Модель»');
        assert.ok(html.indexOf('value="gemma3:4b"') >= 0,
            'модель, которая есть только на одном узле, тоже должна быть в списке');
        // R181: заголовок живёт в ШАПКЕ карточки (index.html), а не в панели.
        // Проверяем и наличие в шапке, и ОТСУТСТВИЕ дубля внутри панели: именно
        // двойной заголовок выглядел как ошибка вёрстки на живом скриншоте.
        assert.ok(INDEX_HTML.indexOf('data-i18n="merge.title"') >= 0,
            'заголовок «Объединение бэкендов» должен быть в шапке карточки');
        assert.ok(panel.innerHTML.indexOf('Объединение бэкендов') < 0,
            'внутри панели заголовка быть не должно - он дублировал шапку карточки');
    });

    check('autoMount регистрируется на DOMContentLoaded', function () {
        assert.strictEqual(typeof domListeners['DOMContentLoaded'], 'function',
            'модуль должен сам монтироваться после разбора страницы');
    });

    // --- 4. сборка композиции --------------------------------------------

    check('buildGroup отказывает при одном узле', function () {
        BM.reset();
        BM.handleInput({ target: { id: 'bmBackend', checked: true, getAttribute: function () { return 'node-a'; } } });
        BM.handleInput({ target: { id: 'bmModel', value: 'qwen3-14b' } });
        assert.strictEqual(BM.buildGroup(BM.state), null, 'одного узла мало для раскладки');
    });

    check('buildGroup: мастер, воркер и RPC-эндпоинт воркера', function () {
        BM.reset();
        BM.handleInput({ target: { id: 'bmModel', value: 'qwen3-14b' } });
        BM.handleInput({ target: { id: 'bmBackend', checked: true, getAttribute: function () { return 'node-a'; } } });
        BM.handleInput({ target: { id: 'bmBackend', checked: true, getAttribute: function () { return 'node-b'; } } });
        BM.handleInput({ target: { id: 'bmMaster', checked: true, getAttribute: function () { return 'node-b'; } } });
        BM.handleInput({ target: { id: 'bmStrategy', value: 'row' } });

        const group = BM.buildGroup(BM.state);
        assert.ok(group, 'композиция должна собираться');
        assert.strictEqual(group.id, 'group-qwen3-14b');
        assert.strictEqual(group.model, 'qwen3-14b');
        assert.strictEqual(group.strategy, 'row');
        assert.strictEqual(group.members.length, 2);
        const master = group.members.filter(function (m) { return m.role === 'master'; });
        assert.strictEqual(master.length, 1, 'мастер ровно один');
        assert.strictEqual(master[0].backendId, 'node-b', 'мастер — выбранный узел');
        const worker = group.members.filter(function (m) { return m.role === 'worker'; })[0];
        assert.strictEqual(worker.backendId, 'node-a');
        assert.strictEqual(worker.rpcEndpoint, '192.0.2.10:50052', 'воркеру нужен адрес RPC-сервера');
    });

    // --- 5. проверка (dry-run) -------------------------------------------

    const planBody = {
        status: 200,
        body: {
            success: true, discoveredModels: 1,
            plans: [{
                model: 'qwen3-14b', strategy: 'sharded',
                reason: 'ни один узел не вмещает модель, а сумма по 2 узлам — вмещает',
                traffic: {
                    layer: { known: true, tokenOverheadPercent: 0.22, prefillMs: 37.4 },
                    row: { known: true, tokenOverheadPercent: 10.5, prefillMs: 1198 },
                },
            }],
            placement: {}, backendGroups: [], warnings: [], hint: '', lang: 'ru',
        },
    };
    const validateBody = {
        status: 200,
        body: {
            success: true, dryRun: true, hasErrors: true,
            issues: [{
                check: 'vram_sum_insufficient', level: 'error', text: 'нужно ≈10.0 ГБ, доступно 4.0 ГБ по 2 узл(ам)',
            }],
            errors: [], warnings: [], hints: [],
        },
    };

    await checkAsync('check(): ровно два запроса — план и валидатор, числа в панели', async function () {
        BM.reset();
        BM.handleInput({ target: { id: 'bmModel', value: 'qwen3-14b' } });
        BM.handleInput({ target: { id: 'bmBackend', checked: true, getAttribute: function () { return 'node-a'; } } });
        BM.handleInput({ target: { id: 'bmBackend', checked: true, getAttribute: function () { return 'node-b'; } } });

        routes = {
            'GET /api/v1/backends': backendList,
            'POST /api/v1/placement/plan': planBody,
            'POST /api/v1/placement/validate': validateBody,
        };
        calls.length = 0;
        const out = await BM.check();
        assert.ok(out, 'check должен вернуть результат');
        assert.strictEqual(calls.length, 2, 'проверка — это план + валидатор, без лишних запросов');
        const validateCall = calls.filter(function (c) { return c.url.indexOf('/validate') >= 0; })[0];
        assert.strictEqual(validateCall.body.lang, 'ru');
        assert.strictEqual(validateCall.body.group.members.length, 2, 'композиция уходит на сервер');
        assert.ok(validateCall.body.group.members.some(function (m) { return m.rpcEndpoint; }),
            'адрес RPC воркера должен уходить на сервер (иначе он его не проверит)');

        assert.ok(panel.innerHTML.indexOf('10.5') >= 0, 'процент row-раскладки должен быть виден');
        assert.ok(panel.innerHTML.indexOf('1.2') >= 0, 'секунды на префилл должны быть видны');
        assert.ok(panel.innerHTML.indexOf('нужно ≈10.0 ГБ') >= 0, 'замечание с числами должно доехать');
        assert.ok(panel.innerHTML.indexOf('не хватает VRAM по узлам') >= 0, 'заголовок замечания переведён');
    });

    // ── R166 (2026-10-17): канал между узлами — ключевой параметр раскладки ────
    //
    // Живой контекст: оценка трафика брала МБ/с только из тела запроса, а без него
    // подставляла 112 МБ/с (1 Гбит/с). На канале 10 Гбит/с это в 10 раз
    // пессимистичнее — композиция могла быть отклонена «из-за сети». Теперь
    // значение можно задать в панели, а источник («оператор / автозамер / дефолт»)
    // виден в подсказке.

    check('панель объединения содержит явное поле канала и кнопку замера', function () {
        const html = BM.panelHtml({
            selected: {}, model: '', strategy: 'layer', bandwidthMbps: 0, linkHint: '',
            backends: [], groups: [], result: null, plan: null, report: null,
        });
        assert.ok(html.indexOf('id="bmLinkMbps"') >= 0, 'поля канала нет в панели');
        assert.ok(html.indexOf('id="bmMeasure"') >= 0, 'кнопки «Замерить» нет в панели');
    });

    await checkAsync('check(): значение канала оператора уходит в оба запроса', async function () {
        BM.reset();
        BM.handleInput({ target: { id: 'bmModel', value: 'qwen3-14b' } });
        BM.handleInput({ target: { id: 'bmBackend', checked: true, getAttribute: function () { return 'node-a'; } } });
        BM.handleInput({ target: { id: 'bmBackend', checked: true, getAttribute: function () { return 'node-b'; } } });
        // 800 Мбит/с = 100 МБ/с — именно в МБ/с ждёт сервер.
        BM.handleInput({ target: { id: 'bmLinkMbps', value: '800' } });
        routes = {
            'GET /api/v1/backends': backendList,
            'POST /api/v1/placement/plan': planBody,
            'POST /api/v1/placement/validate': validateBody,
        };
        calls.length = 0;
        await BM.check();
        const planCall = calls.filter(function (c) { return c.url.indexOf('/plan') >= 0; })[0];
        const validateCall = calls.filter(function (c) { return c.url.indexOf('/validate') >= 0; })[0];
        assert.strictEqual(planCall.body.bandwidthMBps, 100, 'план должен получить 100 МБ/с из 800 Мбит/с');
        assert.strictEqual(validateCall.body.bandwidthMBps, 100, 'валидатор должен получить то же число');

        // Пустое поле = «авто»: 0, а не выдуманное число.
        BM.handleInput({ target: { id: 'bmLinkMbps', value: '' } });
        calls.length = 0;
        await BM.check();
        const planAuto = calls.filter(function (c) { return c.url.indexOf('/plan') >= 0; })[0];
        assert.strictEqual(planAuto.body.bandwidthMBps, 0, 'пустое поле обязано значить «реши сам», а не 0 Мбит/с');
    });

    // --- 6-7. применение -------------------------------------------------

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
            },
            backendGroups: [
                { id: 'group-чужая', model: 'чужая-модель', strategy: 'layer', members: [] },
                { id: 'group-qwen3-14b', model: 'qwen3-14b', strategy: 'layer', members: [] },
            ],
            // R104: состояние групп — источник списка «что будет выгружено».
            groups: [
                {
                    id: 'group-qwen3-14b', model: 'qwen3-14b', strategy: 'layer', usable: true,
                    loadedModels: ['gemma-3-4b', 'qwen3-14b'],
                    members: [
                        { backendId: 'node-a', role: 'master', loadedModels: ['qwen3-14b'] },
                        { backendId: 'node-b', role: 'worker', loadedModels: ['gemma-3-4b'] },
                    ],
                },
            ],
        },
    };

    await checkAsync('apply(): GET → PUT и чужие правила/группы не теряются', async function () {
        BM.reset();
        BM.handleInput({ target: { id: 'bmModel', value: 'qwen3-14b' } });
        BM.handleInput({ target: { id: 'bmBackend', checked: true, getAttribute: function () { return 'node-a'; } } });
        BM.handleInput({ target: { id: 'bmBackend', checked: true, getAttribute: function () { return 'node-b'; } } });

        routes = {
            'GET /api/v1/backends': backendList,
            'GET /api/v1/placement': currentState,
            'PUT /api/v1/placement': { status: 200, body: { success: true, applied: true, persisted: true, groups: [] } },
        };
        calls.length = 0;
        const res = await BM.apply();
        assert.strictEqual(res.applied, true);
        assert.strictEqual(calls.length, 2, 'применение = чтение текущего состояния + запись');
        const put = calls[1];
        assert.strictEqual(put.method, 'PUT');
        assert.strictEqual(put.url, 'http://balancer.test:18081/api/v1/placement');

        const models = put.body.placement.models.map(function (r) { return r.model; });
        assert.ok(models.indexOf('чужая-модель') >= 0,
            'чужое правило обязано сохраниться: PUT заменяет секцию целиком');
        assert.strictEqual(models.filter(function (m) { return m === 'qwen3-14b'; }).length, 1,
            'правило целевой модели должно быть ровно одно');
        const rule = put.body.placement.models.filter(function (r) { return r.model === 'qwen3-14b'; })[0];
        assert.strictEqual(rule.strategy, 'auto', 'правило пишется как auto (адаптивность)');
        assert.deepStrictEqual(rule.auto.prefer, ['sharded', 'single']);

        const groups = put.body.backendGroups.map(function (g) { return g.id; });
        assert.ok(groups.indexOf('group-чужая') >= 0, 'чужая группа обязана сохраниться');
        assert.strictEqual(groups.filter(function (g) { return g === 'group-qwen3-14b'; }).length, 1,
            'своя группа не должна дублироваться');
        assert.ok(panel.innerHTML.indexOf('Политика применена') >= 0, 'оператор должен видеть результат');
        assert.ok(panel.innerHTML.indexOf('Конфиг сохранён на диск') >= 0);
    });

    await checkAsync('apply(): persisted=false показывается честно', async function () {
        BM.reset();
        BM.handleInput({ target: { id: 'bmModel', value: 'qwen3-14b' } });
        BM.handleInput({ target: { id: 'bmBackend', checked: true, getAttribute: function () { return 'node-a'; } } });
        BM.handleInput({ target: { id: 'bmBackend', checked: true, getAttribute: function () { return 'node-b'; } } });

        routes = {
            'GET /api/v1/backends': backendList,
            'GET /api/v1/placement': currentState,
            'PUT /api/v1/placement': {
                status: 200,
                body: {
                    success: true, applied: true, persisted: false,
                    persistError: 'read-only file system: config/config.json',
                    groups: [],
                },
            },
        };
        await BM.apply();
        assert.ok(panel.innerHTML.indexOf('НЕ сохранён') >= 0,
            'нельзя показывать успех там, где файл не записан');
        assert.ok(panel.innerHTML.indexOf('read-only') >= 0, 'причина должна быть видна');
    });

    await checkAsync('apply(): отказ сервера показывается, а не проглатывается', async function () {
        BM.reset();
        BM.handleInput({ target: { id: 'bmModel', value: 'qwen3-14b' } });
        BM.handleInput({ target: { id: 'bmBackend', checked: true, getAttribute: function () { return 'node-a'; } } });
        BM.handleInput({ target: { id: 'bmBackend', checked: true, getAttribute: function () { return 'node-b'; } } });

        routes = {
            'GET /api/v1/backends': backendList,
            'GET /api/v1/placement': currentState,
            'PUT /api/v1/placement': {
                status: 400,
                body: { success: false, error: 'политика не применена: конфиг не проходит валидацию', warnings: ['backendGroups[0]: в группе ровно один master, а найдено 0'] },
            },
        };
        let failed = false;
        try {
            await BM.apply();
        } catch (e) {
            failed = true;
        }
        assert.ok(failed, 'ошибка должна быть выброшена для вызывающего');
        assert.ok(panel.innerHTML.indexOf('Применить не удалось') >= 0);
        assert.ok(panel.innerHTML.indexOf('ровно один master') >= 0, 'причина отказа сервера должна быть видна');
    });

    // --- 8. разъединение -------------------------------------------------

    await checkAsync('split(): снимаются и группа, и правило её модели', async function () {
        BM.reset();
        routes = {
            'GET /api/v1/backends': backendList,
            'GET /api/v1/placement': currentState,
            'PUT /api/v1/placement': { status: 200, body: { success: true, applied: true, persisted: true, groups: [] } },
        };
        calls.length = 0;
        confirmAnswer = true;
        await BM.split('group-qwen3-14b');

        const put = calls[1];
        const groups = put.body.backendGroups.map(function (g) { return g.id; });
        assert.deepStrictEqual(groups, ['group-чужая'], 'снятая группа не должна остаться в конфиге');
        const models = put.body.placement.models.map(function (r) { return r.model; });
        assert.ok(models.indexOf('qwen3-14b') < 0,
            'правило модели снятой группы обязано уйти: иначе после разъединения запросы получат 503');
        assert.ok(models.indexOf('чужая-модель') >= 0, 'чужие правила не трогаем');
    });

    await checkAsync('split(): оператор видит, ЧТО будет выгружено', async function () {
        BM.reset();
        routes = {
            'GET /api/v1/backends': backendList,
            'GET /api/v1/placement': currentState,
            'PUT /api/v1/placement': { status: 200, body: { success: true, applied: true, persisted: true, groups: [] } },
        };
        confirmMessages.length = 0;
        confirmAnswer = true;
        await BM.split('group-qwen3-14b');

        assert.strictEqual(confirmMessages.length, 1, 'подтверждение должно быть ровно одно');
        const message = confirmMessages[0];
        assert.ok(message.indexOf('Будет выгружено') >= 0,
            'до подтверждения оператор обязан узнать, что потеряет: ' + message);
        assert.ok(message.indexOf('qwen3-14b') >= 0 && message.indexOf('gemma-3-4b') >= 0,
            'в списке должны быть обе загруженные модели: ' + message);
    });

    await checkAsync('split(): после разъединения показано, что снято вместе с группой', async function () {
        BM.reset();
        routes = {
            'GET /api/v1/backends': backendList,
            'GET /api/v1/placement': currentState,
            'PUT /api/v1/placement': { status: 200, body: { success: true, applied: true, persisted: true, groups: [] } },
        };
        confirmAnswer = true;
        await BM.split('group-qwen3-14b');
        // R107: removedRules считались, но не показывались — оператор не видел, что
        // вместе с группой снято и правило модели.
        assert.ok(panel.innerHTML.indexOf('Вместе с группой снято правило модели') >= 0,
            'оператор должен видеть, что снято вместе с группой: ' + panel.innerHTML);
    });

    await checkAsync('split(): отказ оператора ничего не записывает', async function () {
        BM.reset();
        routes = {
            'GET /api/v1/backends': backendList,
            'GET /api/v1/placement': currentState,
            'PUT /api/v1/placement': { status: 200, body: { success: true } },
        };
        calls.length = 0;
        confirmAnswer = false;
        await BM.split('group-qwen3-14b');
        const puts = calls.filter(function (c) { return c.method === 'PUT'; });
        assert.strictEqual(puts.length, 0, 'без подтверждения записывать нельзя');
        assert.strictEqual(calls.length, 1, 'чтение состояния до подтверждения — единственный запрос');
        confirmAnswer = true;
    });

    await checkAsync('смена языка перерисовывает панель без перезагрузки', async function () {
        BM.reset();
        routes = {
            'GET /api/v1/backends': {
                status: 200,
                body: { backends: [
                    { id: 'node-a', host: 'h1', type: 'llama_cpp' },
                    { id: 'node-b', host: 'h2', type: 'llama_cpp' },
                ] },
            },
        };
        currentLang = 'ru';
        await BM.mount('backendMergePanel');
        assert.ok(panel.innerHTML.indexOf('Выберите два и более') >= 0,
            'до переключения панель должна быть на RU: ' + panel.innerHTML.slice(0, 120));

        currentLang = 'en';
        assert.strictEqual(typeof winListeners['i18n:changed'], 'function',
            'модуль обязан подписаться на i18n:changed (иначе блок живёт до reload)');
        winListeners['i18n:changed']();

        assert.ok(panel.innerHTML.indexOf('Pick two or more') >= 0,
            'после смены языка должна быть EN-подсказка: ' + panel.innerHTML.slice(0, 160));
        assert.ok(panel.innerHTML.indexOf('Выберите два и более') < 0,
            'RU-текст не должен остаться после переключения');
        assert.ok(panel.innerHTML.indexOf('Node') >= 0, 'заголовки таблицы тоже переводим');
        currentLang = 'ru';
    });

    // --- итог -------------------------------------------------------------

    console.log('\n' + passed + ' passed, ' + failures.length + ' failed');
    if (missingKeys.length) {
        console.log('Не найденные ключи: ' + missingKeys.join(', '));
    }
    if (failures.length) {
        failures.forEach(function (f) { console.error('  - ' + f); });
        process.exit(1);
    }
})();
