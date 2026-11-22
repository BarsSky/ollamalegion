// monitor-groups-lazy-r687.test.js — R687: `POST /api/v1/placement/plan` из
// карточки логических бэкендов уходит ТОЛЬКО когда монитор реально виден.
//
// ЖИВОЙ ФАКТ (CDP-замер на стенде, загрузка дашборда 10 с). Карточка живёт в
// monitor.html (iframe), а iframe создаётся при инициализации панели и остаётся
// скрытым, пока оператор не откроет раздел «Монитор»; plan же грузился на каждой
// загрузке панели. После R684 он отвечает 200, то есть это лишняя работа, а не
// ошибка. Данные карточки (группы, узлы, VRAM) приходят из опроса монитора; из
// plan — только числа трафика.
//
// ЧТО ПРОВЕРЯЕТСЯ:
//   1. autoMount НЕ ходит за планом: рисует карточку и ставит ДВА наблюдателя
//      внутри документа монитора — на карточку и на корень документа (у iframe
//      свой viewport, поэтому наблюдение из родителя не заменило бы это);
//   2. невидимая карточка/скрытый раздел не запрашивают план; скрытая вкладка
//      (document.visibilityState==='hidden') тоже откладывает запрос;
//   3. первый показ раздела «Монитор» запрос ОТПРАВЛЯЕТ, ровно один, с телом
//      {lang}; числа трафика после него появляются в карточке — ленивость не
//      стала отсутствием данных;
//   4. повторный показ второго запроса не делает (план не опрашивается циклом);
//   5. тики монитора (renderFromPlacement) по-прежнему не ходят в сеть;
//   6. появление самой карточки в кадре — самостоятельный триггер (когда
//      оператор скроллит к ней, числа уже не ждут запроса);
//   7. браузер без IntersectionObserver ведёт себя как раньше — один запрос
//      сразу (данные важнее экономии запроса).
//
// КОНТРФАКТУАЛЬНОСТЬ (проверено прогоном на `git show HEAD:…monitor-groups.js`):
// на старом коде пункт 1 падает — autoMount отправляет POST сразу, без всякой
// видимости. См. отчёт: прогон этого файла на исходном модуле даёт 6 красных
// проверок.
//
// Запуск: node webui/tests/monitor-groups-lazy-r687.test.js

'use strict';

const assert = require('assert');
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

// --- стенд: mock DOM + mock fetch + записывающий IntersectionObserver ------

const MODULE_PATH = path.join(__dirname, '..', 'js', 'modules', 'monitor-groups.js');

const panel = { id: 'monitorGroupsPanel', innerHTML: '' };
const documentElement = { id: 'documentElement' };
const docListeners = {};
const requests = [];
let routes = {};

global.window = global;
global.document = {
    // 'loading' — модуль только регистрирует DOMContentLoaded и НЕ монтируется
    // сам: моментом авто-монтирования управляет тест (autoMount вызывается явно).
    readyState: 'loading',
    visibilityState: 'visible',
    documentElement: documentElement,
    getElementById: function (id) { return id === 'monitorGroupsPanel' ? panel : null; },
    addEventListener: function (name, fn) {
        if (!docListeners[name]) { docListeners[name] = []; }
        docListeners[name].push(fn);
    },
    querySelectorAll: function () { return []; },
};
global.localStorage = {
    getItem: function () { return null; },
    setItem: function () {},
    removeItem: function () {},
};
global.I18N = {
    getLang: function () { return 'ru'; },
    t: function (key) { return key; },
};
global.Api = { getAuthHeaders: function () { return { 'Content-Type': 'application/json' }; } };
global.WEBUI_CONFIG = { API_BASE: 'http://balancer.test:18081' };

// FakeIntersectionObserver — стаб с фиксацией вызова: настоящего браузера в
// node нет, а проверить надо именно связку «наблюдатель → запрос». targets
// запоминает цель, fire() доставляет запись так же, как это делает браузер.
const observers = [];
function FakeIntersectionObserver(callback, options) {
    this.callback = callback;
    this.options = options || {};
    this.targets = [];
    observers.push(this);
}
FakeIntersectionObserver.prototype.observe = function (target) { this.targets.push(target); };
FakeIntersectionObserver.prototype.disconnect = function () { this.disconnected = true; };
FakeIntersectionObserver.prototype.fire = function (isIntersecting) {
    this.callback([{ isIntersecting: !!isIntersecting, target: this.targets[0] }], this);
};
global.IntersectionObserver = FakeIntersectionObserver;

global.fetch = function (url, opts) {
    const method = (opts && opts.method) || 'GET';
    requests.push({
        method: method,
        url: url,
        body: opts && opts.body ? JSON.parse(opts.body) : null,
        headers: (opts && opts.headers) || {},
    });
    const key = method + ' ' + String(url).replace('http://balancer.test:18081', '');
    const handler = routes[key];
    if (!handler) {
        return Promise.resolve({
            ok: false, status: 404,
            text: function () { return Promise.resolve('{"error":"no route"}'); },
        });
    }
    const res = typeof handler === 'function' ? handler() : handler;
    return Promise.resolve({
        ok: res.status >= 200 && res.status < 300,
        status: res.status,
        text: function () { return Promise.resolve(JSON.stringify(res.body)); },
    });
};

function planRequests() {
    return requests.filter(function (r) { return r.url.indexOf('/api/v1/placement/plan') >= 0; });
}
function flush() {
    return new Promise(function (resolve) { setTimeout(resolve, 0); });
}
function loadModule() {
    delete require.cache[require.resolve(MODULE_PATH)];
    return require(MODULE_PATH);
}
function cardObserver() {
    return observers.filter(function (o) { return o.targets[0] === panel; })[0];
}
function rootObserver() {
    return observers.filter(function (o) { return o.targets[0] === documentElement; })[0];
}
function fireVisibilityChange() {
    (docListeners['visibilitychange'] || []).forEach(function (fn) { fn(); });
}

const PLAN_OK = {
    status: 200,
    body: { plans: [{ model: 'qwen3-14b', traffic: { layer: { known: true, tokenOverheadPercent: 0.22, prefillMs: 37.4 } } }] },
};
const PLACEMENT_OK = {
    status: 200,
    body: { groups: [{ id: 'group-qwen3-14b', model: 'qwen3-14b', strategy: 'layer', usable: true, healthy: true, slots: 1, vramFreeMB: 8192, vramTotalMB: 16384, tokensKnown: true, tokensPerSecond: 18.4 }] },
};

(async function run() {
    routes = {
        'GET /api/v1/placement': PLACEMENT_OK,
        'POST /api/v1/placement/plan': PLAN_OK,
    };

    const MG = loadModule();

    // --- 1. монтирование без запроса ------------------------------------
    await checkAsync('autoMount: карточка рисуется, POST плана НЕ уходит', async function () {
        requests.length = 0;
        observers.length = 0;
        MG.reset();
        MG.autoMount();
        await flush();
        assert.strictEqual(planRequests().length, 0,
            'запрос плана на загрузке страницы — ровно тот лишний POST, который закрывает R687');
        assert.ok(panel.innerHTML.indexOf('monitorGroups.title') >= 0,
            'карточка отрисована ещё до всякого плана');
    });

    check('autoMount ставит наблюдателей на карточку И на корень документа монитора', function () {
        assert.strictEqual(observers.length, 2, 'нужны оба сигнала видимости: карточка и раздел');
        assert.ok(cardObserver(), 'наблюдатель карточки не поставлен');
        assert.ok(rootObserver(), 'наблюдатель корня документа не поставлен (первый показ раздела)');
        assert.strictEqual(cardObserver().options.threshold, 0, 'достаточно появления в кадре');
        assert.strictEqual(rootObserver().options.threshold, 0, 'достаточно появления раздела');
    });

    // --- 2. невидимый монитор -------------------------------------------
    await checkAsync('скрытый раздел и скрытая карточка план не запрашивают', async function () {
        requests.length = 0;
        cardObserver().fire(false);
        rootObserver().fire(false);
        await flush();
        assert.strictEqual(planRequests().length, 0, 'ни карточки, ни раздела не видно — запроса быть не должно');
    });

    await checkAsync('скрытая вкладка откладывает запрос даже при показанном разделе', async function () {
        requests.length = 0;
        global.document.visibilityState = 'hidden';
        rootObserver().fire(true);
        await flush();
        assert.strictEqual(planRequests().length, 0, 'visibilityState=hidden — запрос откладывается');
    });

    // --- 3. первый показ: запрос и данные -------------------------------
    await checkAsync('первый показ страницы «Монитор»: ровно один POST плана и числа в карточке', async function () {
        global.document.visibilityState = 'visible';
        // Группы приходят из опроса монитора (renderFromPlacement), план несёт
        // только числа трафика — ровно как на живом мониторе.
        MG.renderFromPlacement(PLACEMENT_OK.body);
        requests.length = 0;
        assert.ok((docListeners['visibilitychange'] || []).length > 0,
            'ожидание видимости должно опираться на visibilitychange');
        fireVisibilityChange();
        await flush();
        const posts = planRequests();
        assert.strictEqual(posts.length, 1, 'при показе раздела план обязан загрузиться — иначе ленивость стала бы отсутствием данных');
        assert.strictEqual(posts[0].method, 'POST');
        assert.deepStrictEqual(posts[0].body, { lang: 'ru' }, 'тело то же, что раньше');
        assert.ok(panel.innerHTML.indexOf('plan.strategy.layer') >= 0, 'строка трафика появилась в карточке');
        assert.ok(panel.innerHTML.indexOf('0.2 %') >= 0, 'числа трафика из плана отрисованы');
    });

    // --- 4. без повторов ------------------------------------------------
    await checkAsync('повторный показ не делает второй POST', async function () {
        rootObserver().fire(false);
        rootObserver().fire(true);
        cardObserver().fire(true);
        await flush();
        assert.strictEqual(planRequests().length, 1, 'план не опрашивается циклом: числа меняются редко');
    });

    // --- 5. тики монитора в сеть не ходят -------------------------------
    await checkAsync('renderFromPlacement (тики монитора) не ходит в сеть', async function () {
        requests.length = 0;
        MG.renderFromPlacement(PLACEMENT_OK.body);
        MG.renderFromPlacement(PLACEMENT_OK.body);
        await flush();
        assert.strictEqual(requests.length, 0, 'карточка рисуется из данных опроса монитора');
    });

    // --- 6. карточка в кадре — самостоятельный триггер ------------------
    await checkAsync('появление карточки в кадре тоже запрашивает план', async function () {
        requests.length = 0;
        observers.length = 0;
        const MG2 = loadModule();
        MG2.reset();
        MG2.autoMount();
        await flush();
        assert.strictEqual(planRequests().length, 0, 'до появления карточки запроса нет');
        cardObserver().fire(true);
        await flush();
        assert.strictEqual(planRequests().length, 1,
            'оператор доскроллил до карточки — числа должны подгрузиться, даже если раздел «показан» не заметили');
    });

    // --- 7. браузер без IntersectionObserver ----------------------------
    await checkAsync('без IntersectionObserver план запрашивается сразу (как до R687)', async function () {
        const saved = global.IntersectionObserver;
        delete global.IntersectionObserver;
        try {
            requests.length = 0;
            const MG3 = loadModule();
            MG3.reset();
            MG3.autoMount();
            await flush();
            assert.strictEqual(planRequests().length, 1,
                'в браузере без наблюдателя данные должны подгружаться сразу — иначе карточка осталась бы без трафика');
        } finally {
            global.IntersectionObserver = saved;
        }
    });

    check('модуль не заводит свой цикл и не трогает postMessage-паузу монитора', function () {
        const src = require('fs').readFileSync(MODULE_PATH, 'utf8');
        assert.ok(src.indexOf('setInterval') < 0, 'карточка не должна опрашивать план циклом');
        assert.ok(src.indexOf('postMessage') < 0, 'пауза iframe (R493/R679) живёт в state.js/app.js, не здесь');
    });

    console.log('\n' + passed + ' passed, ' + failures.length + ' failed');
    if (failures.length) {
        failures.forEach(function (f) { console.error('  - ' + f); });
        process.exit(1);
    }
})();
