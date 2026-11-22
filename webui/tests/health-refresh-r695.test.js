// health-refresh-r695.test.js — R695: кнопка «Обновить» в шапке панели обновляет
// страницу «Состояние» (iframe) РОВНО ОДНИМ немедленным запросом.
//
// ЖИВОЙ ДЕФЕКТ. На странице «Состояние» своих запросов у панели нет: данные тянет
// сам health.html своим циклом (js/modules/health-poll.js, 5 с). Провайдер
// DataRefresh 'health' переотправлял в iframe только конфиг и состояние паузы,
// поэтому клик по единственной кнопке «Обновить» (#refreshBtn в шапке) НЕ
// приводил ни к одному запросу /api/v1/health/detailed — оператор видел «кнопка
// не работает» (ровно класс R84). Своя кнопка на самой странице работала, но это
// второй интерфейс там, где приложение договорилось об одном (R493).
//
// ЧТО СДЕЛАНО. Добавлено сообщение ollamalegion-refresh (тот же канал, что
// ollamalegion-pause/ollamalegion-config), health.html обрабатывает его через
// существующий контракт цикла — poll.refreshNow() (health-poll.js:157: снять
// таймер, запросить сразу, поставить ОДИН новый).
//
// ПОЧЕМУ СООБЩЕНИЕ ШЛЁТ КЛИК, А НЕ ПРОВАЙДЕР DataRefresh. Провайдер страницы
// вызывается не только кнопкой: тот же DataRefresh.refresh(page) дёргает тик
// авто-обновления. Сообщение из провайдера уходило бы каждые 5 секунд и давало
// ВТОРОЙ опрос рядом с собственным циклом страницы — это как раз то, чего нельзя.
// Тест проверяет обе половины: (1) поведение страницы на подменённых таймерах и
// fetch-стабе, (2) что панель шлёт сообщение только из обработчика клика.
//
// НЕ-ВАКУУМНОСТЬ. `WEBUI_ROOT=<старая статика>` (копия `git show 4613df08:webui`)
// обязан дать КРАСНЫЙ результат: сообщения ollamalegion-refresh там нет ни в
// app-data.js, ни в health.html, поэтому стаб не увидит ни одного запроса.
//
// Запуск: node webui/tests/health-refresh-r695.test.js
//         WEBUI_ROOT=<dir> node webui/tests/health-refresh-r695.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const WEBUI = process.env.WEBUI_ROOT
    ? path.resolve(process.env.WEBUI_ROOT)
    : path.join(__dirname, '..');
const read = (rel) => fs.readFileSync(path.join(WEBUI, rel), 'utf8');

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
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/** codeOf — исходник без комментариев (объяснения дефекта цитируют старый код). */
function codeOf(src) {
    const noBlock = src.replace(/\/\*[\s\S]*?\*\//g, '');
    return noBlock.split('\n').map(function (line) {
        const i = line.indexOf('//');
        return i === -1 ? line : line.slice(0, i);
    }).join('\n');
}

// ===========================================================================
// 1. Панель: сообщение шлётся КЛИКОМ, а не провайдером DataRefresh
// ===========================================================================
const appData = codeOf(read('js/modules/app-data.js'));
const healthHtmlSrc = read('health.html');
const healthCode = codeOf(healthHtmlSrc);

check('1a. панель умеет postHealthRefresh и шлёт тип ollamalegion-refresh', function () {
    const start = appData.indexOf('function postHealthRefresh(');
    assert.ok(start !== -1,
        'в app-data.js нет postHealthRefresh: кнопка «Обновить» в шапке не касается ' +
        'страницы «Состояние» вовсе (ровно живой дефект R695)');
    const body = appData.slice(start, appData.indexOf('function bindHealthRefreshButton', start));
    assert.ok(/type:\s*'ollamalegion-refresh'/.test(body),
        'postHealthRefresh не шлёт type=ollamalegion-refresh');
    assert.ok(/healthFrame/.test(body), 'postHealthRefresh не адресует iframe #healthFrame');
});

check('1b. источник сообщения — клик по #refreshBtn (а не провайдер DataRefresh)', function () {
    const start = appData.indexOf('function bindHealthRefreshButton(');
    assert.ok(start !== -1, 'нет bindHealthRefreshButton: некому отправить сообщение');
    const body = appData.slice(start, appData.indexOf('function refreshCurrentPage', start));
    assert.ok(/getElementById\('refreshBtn'\)/.test(body),
        'bindHealthRefreshButton не привязан к кнопке #refreshBtn в шапке');
    assert.ok(/addEventListener\('click'/.test(body), 'на кнопке нет обработчика клика');
    assert.ok(/postHealthRefresh\(\)/.test(body), 'обработчик клика не шлёт сообщение странице');
    assert.ok(/getCurrentPage\(\)\s*!==\s*'health'/.test(body),
        'нет проверки «открыта страница health»: сообщение летело бы на любой странице');
});

check('1c. провайдер DataRefresh для health НЕ шлёт refresh (иначе тик = второй опрос)', function () {
    const start = appData.indexOf("DR.register('health'");
    assert.ok(start !== -1, "нет провайдера 'health'");
    const body = appData.slice(start, appData.indexOf("DR.register('settings'", start));
    assert.strictEqual(/postHealthRefresh\(\)/.test(body), false,
        'провайдер health шлёт ollamalegion-refresh: тик авто-обновления (5 с) стал бы ' +
        'ВТОРЫМ опросом /health/detailed рядом с собственным циклом страницы');
    assert.ok(/sendHealthConfig\(\)/.test(body),
        'провайдер health перестал переотправлять конфиг/паузу (регресс R690)');
    assert.ok(/bindHealthRefreshButton\(\)/.test(appData),
        'bindHealthRefreshButton нигде не вызывается: обработчик кнопки не навесится');
});

check('1d. health.html обрабатывает ollamalegion-refresh через poll.refreshNow()', function () {
    assert.ok(/d\.type\s*===\s*'ollamalegion-refresh'/.test(healthCode),
        'health.html не обрабатывает ollamalegion-refresh: сообщение панели игнорируется');
    const start = healthCode.indexOf('function refreshFromPanel(');
    assert.ok(start !== -1, 'нет refreshFromPanel: непонятно, что делать на сообщение');
    const body = healthCode.slice(start, healthCode.indexOf('function loadNow', start));
    assert.ok(/poll\.refreshNow\(\)/.test(body),
        'сообщение не использует существующий контракт цикла (health-poll.js refreshNow)');
    assert.ok(/refreshSkipped\+\+/.test(body) && /refreshAccepted\+\+/.test(body),
        'нет защиты от дублей: повторное сообщение даст второй запрос и переставит таймер');
});

// ===========================================================================
// 2. Страница: поведение на подменённых таймерах и стабе fetch
// ===========================================================================

/** inlineScripts — все inline <script> документа, в порядке появления. */
function inlineScripts(html) {
    const out = [];
    const re = /<script(?![^>]*\bsrc=)[^>]*>([\s\S]*?)<\/script>/gi;
    let m;
    while ((m = re.exec(html)) !== null) out.push(m[1]);
    return out;
}

/**
 * bootPage — health.html в песочнице: настоящие inline-скрипты страницы,
 * настоящий js/modules/health-poll.js, но БЕЗ браузера: DOM-заглушки, стаб fetch
 * со счётчиком запросов и подменённые таймеры (setTimeout/clearTimeout).
 *
 * Так проверяется ровно то, что нельзя проверить регуляркой: сколько запросов
 * ушло, сколько таймеров стоит и что происходит на повторное сообщение.
 */
function bootPage() {
    const timers = new Map();
    let nextTimerId = 1;
    const winListeners = {};
    const elements = {};

    function stub(id) {
        return {
            id: id,
            textContent: '',
            innerHTML: '',
            className: '',
            value: '',
            style: {},
            attrs: {},
            setAttribute: function (k, v) { this.attrs[k] = v; },
            getAttribute: function (k) { return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null; },
            querySelectorAll: function () { return []; },
            addEventListener: function () { },
            classList: { add: function () { }, remove: function () { }, toggle: function () { } }
        };
    }
    function el(id) { if (!elements[id]) elements[id] = stub(id); return elements[id]; }

    const store = {};
    const sandbox = {
        console: console,
        navigator: { language: 'ru' },
        localStorage: {
            getItem: function (k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
            setItem: function (k, v) { store[k] = String(v); },
            removeItem: function (k) { delete store[k]; }
        },
        document: {
            readyState: 'complete',
            visibilityState: 'visible',
            documentElement: stub('#html'),
            body: stub('#body'),
            getElementById: el,
            querySelectorAll: function () { return []; },
            addEventListener: function () { }
        },
        addEventListener: function (t, fn) { (winListeners[t] = winListeners[t] || []).push(fn); },
        removeEventListener: function (t, fn) {
            const a = winListeners[t] || [];
            const i = a.indexOf(fn);
            if (i !== -1) a.splice(i, 1);
        },
        setTimeout: function (fn, ms) { const id = nextTimerId++; timers.set(id, { fn: fn, ms: ms }); return id; },
        clearTimeout: function (id) { timers.delete(id); }
    };
    sandbox.window = sandbox;

    // fetch-стаб: считаем ТОЛЬКО запросы /api/v1/health/detailed.
    const fetches = [];
    sandbox.fetch = function (url) {
        fetches.push(String(url));
        return Promise.resolve({
            ok: true,
            status: 200,
            json: function () {
                return Promise.resolve({
                    status: 'healthy', healthScore: 100, totalBackends: 1, healthyBackends: 1,
                    unhealthyBackends: 0, withAgent: 0, backends: [], recentErrors: [], errorsBySource: {}
                });
            }
        });
    };

    vm.createContext(sandbox);
    const scripts = inlineScripts(read('health.html'));
    assert.ok(scripts.length >= 2, 'в health.html не нашлось inline-скриптов');
    // Порядок как в браузере: head-скрипты (тема, словарь), затем модуль цикла,
    // затем скрипт страницы.
    vm.runInContext(scripts[0], sandbox, { filename: 'health.html#inline-1' });
    vm.runInContext(scripts[1], sandbox, { filename: 'health.html#inline-2' });
    vm.runInContext(read('js/modules/health-poll.js'), sandbox, { filename: 'health-poll.js' });
    for (let i = 2; i < scripts.length; i++) {
        vm.runInContext(scripts[i], sandbox, { filename: 'health.html#inline-' + (i + 1) });
    }

    return {
        sandbox: sandbox,
        fetches: fetches,
        timers: timers,
        /** message — доставить сообщение панели в страницу. */
        message: function (data) {
            (winListeners['message'] || []).slice().forEach(function (fn) {
                fn({ data: data, origin: 'http://panel.test' });
            });
        },
        intervalDelays: function () { return [...timers.values()].map(function (t) { return t.ms; }); }
    };
}

(async function main() {
    const page = bootPage();
    const poll = page.sandbox.healthPoll;
    assert.ok(poll && typeof poll.refreshNow === 'function', 'страница не создала window.healthPoll');

    check('2a. boot страницы: один запрос, один таймер на 5 с (как в панели)', function () {
        assert.strictEqual(page.fetches.length, 1,
            'boot ушёл в API не один раз: ' + page.fetches.length);
        assert.ok(page.fetches[0].indexOf('/api/v1/health/detailed') !== -1,
            'запрошен не /api/v1/health/detailed: ' + page.fetches[0]);
        assert.strictEqual(page.timers.size, 1, 'таймеров на старте не один: ' + page.timers.size);
        assert.deepStrictEqual(page.intervalDelays(), [5000], 'интервал не 5 с: ' + page.intervalDelays());
        assert.strictEqual(poll.stats().loads, 0, 'цикл сделал запрос сам: boot и цикл сходили бы дважды');
    });

    await sleep(20); // дать отработать стабу (микрозадачи промисов)

    check('2b. сообщение ollamalegion-refresh = РОВНО ОДИН немедленный запрос', function () {
        const before = page.fetches.length;
        page.message({ type: 'ollamalegion-refresh' });
        assert.strictEqual(page.fetches.length, before + 1,
            'на сообщение ушло ' + (page.fetches.length - before) + ' запросов вместо одного');
        assert.strictEqual(poll.stats().loads, 1, 'цикл не зафиксировал немедленный запрос');
        assert.strictEqual(page.timers.size, 1,
            'таймеров стало ' + page.timers.size + ' (ожидался один: refreshNow снимает и ставит один)');
        assert.deepStrictEqual(page.intervalDelays(), [5000], 'интервал сбит: ' + page.intervalDelays());
        assert.strictEqual(poll.stats().scheduled, true, 'цикл остался без таймера после ручного обновления');
        assert.strictEqual(poll.stats().ticks, 0, 'запрос пришёл от тика, а не немедленно');
        const st = page.sandbox.healthRefreshStats();
        assert.strictEqual(st.accepted, 1, 'принято сообщений: ' + st.accepted);
        assert.strictEqual(st.skipped, 0, 'первое сообщение отброшено как дубль');
    });

    check('2c. ДУБЛЬ того же сообщения не даёт второго запроса (защита от дублей)', function () {
        const before = page.fetches.length;
        page.message({ type: 'ollamalegion-refresh' });   // запрос ещё «в полёте»
        page.message({ type: 'ollamalegion-refresh' });   // и ещё раз
        assert.strictEqual(page.fetches.length, before,
            'дубль сообщения дал ещё запрос(ы): ' + (page.fetches.length - before));
        assert.strictEqual(page.timers.size, 1, 'дубль завёл второй таймер');
        const st = page.sandbox.healthRefreshStats();
        assert.strictEqual(st.accepted, 1, 'дубли посчитаны как принятые: ' + st.accepted);
        assert.ok(st.skipped >= 2, 'дубли не посчитаны: skipped=' + st.skipped);
    });

    await sleep(450); // выйти за окно дедупликации (REFRESH_DEDUP_MS)

    check('2d. после окна дедупликации ручное обновление снова работает (не «залипло»)', function () {
        const before = page.fetches.length;
        page.message({ type: 'ollamalegion-refresh' });
        assert.strictEqual(page.fetches.length, before + 1,
            'второе осмысленное нажатие проигнорировано: дедупликация залипла навсегда');
        assert.strictEqual(poll.stats().loads, 2, 'цикл не увидел второго ручного запроса');
        assert.strictEqual(page.timers.size, 1, 'после второго нажатия таймеров не один');
    });

    check('2e. пауза/конфиг панели продолжают работать (канал не сломан)', function () {
        page.message({ type: 'ollamalegion-pause', paused: true });
        assert.strictEqual(poll.isPausedBy('remote'), true, 'ollamalegion-pause больше не ставит паузу');
        assert.strictEqual(page.timers.size, 0, 'на паузе остался таймер');
        page.message({ type: 'ollamalegion-pause', paused: false });
        assert.strictEqual(poll.isPausedBy('remote'), false, 'ollamalegion-pause не снимает паузу');
        assert.strictEqual(page.timers.size, 1, 'после снятия паузы цикл не продолжился');
        page.message({ type: 'ollamalegion-config', lang: 'en' });
        assert.strictEqual(page.sandbox.I18N.lang, 'en', 'ollamalegion-config не применяет язык');
    });

    console.log('\n' + (failures.length ? 'FAILED: ' + failures.length : 'OK: ' + passed + ' checks passed'));
    if (failures.length) {
        failures.forEach(function (f) { console.error('  FAIL: ' + f); });
        process.exit(1);
    }
})().catch(function (e) {
    console.error('FATAL', e && e.stack || e);
    process.exit(1);
});
