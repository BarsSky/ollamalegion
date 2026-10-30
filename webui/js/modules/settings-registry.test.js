// settings-registry.test.js — R305 (этап 4 рефакторинга настроек WebUI):
// реестр «горячих» настроек балансера на странице «Настройки».
//
// Запуск: node webui/js/modules/settings-registry.test.js
//
// ЧТО БЫЛО НЕ ТАК ДО ЭТОЙ ПРАВКИ. `grep 'api/v1/settings' webui/**` давал РОВНО
// 0 совпадений: балансер отдаёт реестр из 17 настроек, которые применяются БЕЗ
// рестарта (LB_STREAM_STALL_ABORT_SEC, LB_DONOR_ALLOW_OWN_COPIES,
// LB_PLACE_OVER_RELOAD, LB_LIVE_COPY_PROBE, LB_TOOLCALL_PROSE_*,
// LB_ADMISSION_WAIT_SEC, …), а панель их не показывала и не меняла вовсе.
// Оператор видел только «старые» поля config.json, правил env и не понимал,
// почему значение ни на что не влияет: его перекрывал override, записанный
// раньше. Блок панели (index.html #hotSettingsPanel + settings-registry.js)
// закрывает именно это.
//
// Тест без браузера и без сети: fetch — мок по ключу "METHOD /path" (как в
// backend-merge-dom.test.js), поэтому видно, КАКОЙ запрос ушёл на КАКОЕ действие.
// Проверяется:
//   1. подключение: контейнер и тег модуля на странице «Настройки» (иначе блок
//      просто не появится, а юнит-проверки модуля останутся зелёными);
//   2. рендер: 3 настройки разных групп → строки с effective и правильным бейджем
//      источника (по умолчанию / из env / задано в панели);
//   3. изменение: «Применить» → PUT /api/v1/settings {"name","value":<число>}
//      (bool → 0/1, duration_sec → секунды), строка обновляется ИЗ ОТВЕТА;
//   4. сброс: POST /api/v1/settings/reset {"name"} — и только для override;
//   5. мусор: "" / "abc" / "30s" → PUT НЕ уходит, ошибка видна в строке;
//   6. старый балансер (404 или ответ без поля settings) → «реестр недоступен
//      (старый балансер)», а не пустота и не падение.

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

let passed = 0;
const failures = [];
async function check(name, fn) {
    try {
        await fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' \u2014 ' + (e && e.message));
    }
}

// --- минимальный DOM ---------------------------------------------------------
const DOM = {};
function makeEl(id) {
    const el = {
        id: id || '',
        innerHTML: '',
        textContent: '',
        value: '',
        style: {},
        dataset: {},
        children: [],
        _listeners: {},
        _attrs: {},
        classList: { add() {}, remove() {}, contains() { return false; } },
        addEventListener(ev, fn) { (this._listeners[ev] = this._listeners[ev] || []).push(fn); },
        removeEventListener() {},
        appendChild() {},
        getAttribute(k) { return Object.prototype.hasOwnProperty.call(this._attrs, k) ? this._attrs[k] : null; },
        setAttribute(k, v) { this._attrs[k] = String(v); },
        removeAttribute(k) { delete this._attrs[k]; },
        querySelector() { return null; },
        querySelectorAll() { return []; },
    };
    return el;
}

// Поля, которые «оператор набрал в строке»: контейнер отдаёт их по селектору
// [data-hot-input="..."] — ровно так модуль читает значение в браузере.
const inputValues = {};
function containerEl() {
    const el = makeEl('hotSettingsPanel');
    el.querySelector = function (sel) {
        const m = /^\[data-hot-input="(.+)"\]$/.exec(String(sel));
        if (!m) { return null; }
        if (!Object.prototype.hasOwnProperty.call(inputValues, m[1])) { return null; }
        return { value: String(inputValues[m[1]]) };
    };
    return el;
}
DOM['hotSettingsPanel'] = containerEl();

const domListeners = {};
global.document = {
    // 'loading' — модуль ждёт DOMContentLoaded и НЕ делает запрос при загрузке:
    // сеть в тесте должна зависеть только от явных вызовов.
    readyState: 'loading',
    getElementById(id) { return DOM[id] || null; },
    createElement() { return makeEl(''); },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    addEventListener(ev, fn) { domListeners[ev] = fn; },
};
global.window = global;

// --- сеть: мок fetch по ключу "METHOD /path" --------------------------------
const BASE = 'http://balancer.test:18081';
global.WEBUI_CONFIG = { API_BASE: BASE, API_TOKEN: 'tok-305' };

const calls = [];
let routes = {};
function mkRes(status, body) {
    const text = JSON.stringify(body === undefined ? null : body);
    return {
        ok: status >= 200 && status < 300,
        status: status,
        statusText: status === 200 ? 'OK' : 'Error',
        headers: { get() { return 'application/json'; } },
        text() { return Promise.resolve(text); },
        json() { return Promise.resolve(JSON.parse(text)); },
    };
}
global.fetch = function (url, opts) {
    const method = ((opts && opts.method) || 'GET').toUpperCase();
    const p = String(url).replace(BASE, '');
    const body = (opts && opts.body) ? JSON.parse(opts.body) : null;
    calls.push({ method: method, path: p, body: body, headers: (opts && opts.headers) || {} });
    const handler = routes[method + ' ' + p];
    if (handler === undefined) {
        return Promise.resolve(mkRes(404, { error: 'no route: ' + method + ' ' + p }));
    }
    if (typeof handler === 'function') {
        // Обработчик может вернуть ОБЕЩАНИЕ — так проверяется состояние «ждём
        // ответа» (гашение кнопок), которое иначе проскочило бы как одна тик-задача.
        return Promise.resolve(handler(body)).then(function (res) {
            return mkRes(res.status, res.body);
        });
    }
    return Promise.resolve(mkRes(handler.status, handler.body));
};

// --- i18n: НАСТОЯЩИЕ словари ru.js/en.js -----------------------------------
// ЗАЧЕМ НАСТОЯЩИЕ: если ключ пропал из словаря, панель покажет сырой ключ
// оператору. Тест ловит это как «missingKeys», а не сверяет текст по константе,
// вшитой в тест (та разошлась бы с ru.js на первой же правке формулировки).
require('../i18n/ru.js');
require('../i18n/en.js');
const RU = global.I18N_RU;
const EN = global.I18N_EN;
assert.ok(RU && EN, 'ru.js и en.js должны быть загружены');

const missingKeys = [];
let currentLang = 'ru';
global.I18N = {
    getLang() { return currentLang; },
    t(key, vars) {
        const pack = currentLang === 'ru' ? RU : EN;
        let s = pack[key];
        if (s === undefined) {
            if (missingKeys.indexOf(key) === -1) { missingKeys.push(key); }
            s = key;
        }
        if (vars) {
            Object.keys(vars).forEach(function (p) {
                s = String(s).split('{' + p + '}').join(String(vars[p]));
            });
        }
        return s;
    },
};

// --- модули: api.js (общий путь запросов) + settings-registry.js -------------
require('./api.js');
assert.ok(global.Api && global.Api.hotSettings, 'api.js должен публиковать Api.hotSettings');
require('./settings-registry.js');
const SR = global.SettingsRegistry;
assert.ok(SR, 'window.SettingsRegistry должен быть экспортирован');

// --- фикстура реестра: три группы и три разных источника --------------------
function fixture() {
    return {
        count: 3,
        overrides: 1,
        sourcePriority: ['override', 'env', 'default'],
        settings: [
            {
                name: 'LB_STREAM_STALL_ABORT_SEC', type: 'duration_sec', group: 'streaming',
                description: 'Сколько секунд молчания стрима считать зависанием; 0 = не прерывать.',
                hotReload: true, default: 120, effective: 120, source: 'default',
            },
            {
                name: 'LB_DONOR_ALLOW_OWN_COPIES', type: 'bool', group: 'nctx',
                description: 'Разрешать донору держать свои копии моделей (0/1).',
                hotReload: true, default: 0, effective: 1, source: 'env',
            },
            {
                name: 'LB_ADMISSION_WAIT_SEC', type: 'duration_sec', group: 'queue',
                description: 'Сколько ждать свободный слот, прежде чем ответить 503. 0 = не ждать (прежнее поведение).',
                hotReload: true, default: 300, effective: 60, source: 'override',
            },
        ],
    };
}

function html() { return DOM['hotSettingsPanel'].innerHTML; }
function puts() { return calls.filter(function (c) { return c.method === 'PUT' && c.path === '/api/v1/settings'; }); }
function resets() { return calls.filter(function (c) { return c.method === 'POST' && c.path === '/api/v1/settings/reset'; }); }
function gets() { return calls.filter(function (c) { return c.method === 'GET' && c.path === '/api/v1/settings'; }); }
function rowHtml(htmlText, name) {
    const i = htmlText.indexOf('data-hot-row="' + name + '"');
    assert.ok(i >= 0, 'строки ' + name + ' нет в разметке');
    const next = htmlText.indexOf('data-hot-row="', i + 1);
    return htmlText.slice(i, next < 0 ? htmlText.length : next);
}

// --- 1. подключение к странице «Настройки» ----------------------------------
const MODULE_SRC = fs.readFileSync(path.join(__dirname, 'settings-registry.js'), 'utf8');
const INDEX_HTML = fs.readFileSync(path.join(__dirname, '..', '..', 'index.html'), 'utf8');

(async function main() {
    console.log('settings-registry.test.js — R305 (реестр горячих настроек)\n');

    await check('index.html: контейнер #hotSettingsPanel и тег модуля на месте', function () {
        assert.ok(INDEX_HTML.indexOf('id="hotSettingsPanel"') >= 0,
            'нет контейнера #hotSettingsPanel — блок просто не появится на странице');
        assert.ok(INDEX_HTML.indexOf('js/modules/settings-registry.js') >= 0,
            'нет тега script с модулем');
        assert.ok(INDEX_HTML.indexOf('data-i18n="settings.hot.title"') >= 0,
            'секция реестра не на странице настроек');
        assert.ok(INDEX_HTML.indexOf('id="section-hot-settings"') >= 0,
            'нет секции #section-hot-settings (аккордеон «Горячие настройки»)');
        // Модуль обязан грузиться ПОСЛЕ api.js: запросы идут через Api.hotSettings.
        assert.ok(INDEX_HTML.indexOf('js/modules/api.js') < INDEX_HTML.indexOf('js/modules/settings-registry.js'),
            'settings-registry.js загружен раньше api.js — Api.hotSettings будет undefined');
    });

    // --- 2. рендер: группы, effective, бейджи источника ---------------------
    await check('mount() привязывает панель к контейнеру и рисует «Загрузка…»', function () {
        assert.strictEqual(SR.mount(), true, 'mount() не нашёл контейнер');
        assert.ok(html().indexOf('Загрузка реестра настроек') >= 0,
            'до первого ответа нет честного «Загрузка реестра настроек…»: ' + html().slice(0, 200));
        assert.ok(DOM['hotSettingsPanel']._listeners.click,
            'клики не делегированы на контейнер — «Применить» перестанет работать после перерисовки');
    });

    await check('GET /api/v1/settings уходит через общий Api (auth-заголовки на месте)', async function () {
        calls.length = 0;
        routes = { 'GET /api/v1/settings': { status: 200, body: fixture() } };
        const list = await SR.load();
        assert.strictEqual(gets().length, 1, 'ожидался ровно один GET /api/v1/settings');
        assert.strictEqual(list.length, 3);
        const h = gets()[0].headers;
        assert.strictEqual(h['X-API-Token'], 'tok-305',
            'запрос ушёл без заголовков api.js (break-glass X-API-Token) — модуль собирает заголовки вручную?');
        assert.strictEqual(h['Content-Type'], 'application/json');
    });

    await check('рендер: три группы с человекочитаемыми заголовками и сводка источников', function () {
        const h = html();
        assert.ok(h.indexOf('Стриминг') >= 0, 'нет группы «Стриминг»');
        assert.ok(h.indexOf('Окно контекста') >= 0, 'нет группы «Окно контекста»');
        assert.ok(h.indexOf('Очередь') >= 0, 'нет группы «Очередь»');
        assert.strictEqual(h.split('class="hot-setting-row"').length - 1, 3, 'ожидались три строки настроек');
        // Приоритет берётся из sourcePriority ответа, а не выдуман панелью.
        assert.ok(h.indexOf('Задано в панели: 1 из 3') >= 0, 'нет сводки «сколько задано в панели»');
        assert.ok(h.indexOf('задано в панели → из env → по умолчанию') >= 0,
            'не показан приоритет источников из sourcePriority: ' + h.slice(0, 400));
    });

    await check('рендер: имя (моно), описание с сервера и ПРИМЕНЁННОЕ значение', function () {
        const h = html();
        assert.ok(h.indexOf('<code class="hot-setting-name">LB_STREAM_STALL_ABORT_SEC</code>') >= 0,
            'имя настройки не в моно-стиле');
        assert.ok(h.indexOf('Сколько ждать свободный слот, прежде чем ответить 503') >= 0,
            'описание пришло не с сервера / потерялось');
        assert.ok(h.indexOf('data-hot-effective="LB_STREAM_STALL_ABORT_SEC"') >= 0);
        assert.ok(h.indexOf('120 сек') >= 0, 'effective duration_sec без единицы «сек»');
        assert.ok(h.indexOf('1 (вкл)') >= 0, 'bool effective не показан как «1 (вкл)»');
        assert.ok(h.indexOf('60 сек') >= 0, 'effective override-значение не показано');
    });

    await check('рендер: бейдж источника — «по умолчанию» / «из env» / «задано в панели»', function () {
        const h = html();
        assert.ok(h.indexOf('class="badge badge-info" data-hot-badge="LB_STREAM_STALL_ABORT_SEC">по умолчанию</span>') >= 0,
            'source=default не подписан «по умолчанию» штатным бейджем');
        assert.ok(h.indexOf('class="badge badge-warning" data-hot-badge="LB_DONOR_ALLOW_OWN_COPIES">из env</span>') >= 0,
            'source=env не подписан «из env»');
        assert.ok(h.indexOf('class="badge badge-success" data-hot-badge="LB_ADMISSION_WAIT_SEC">задано в панели</span>') >= 0,
            'source=override не подписан «задано в панели»');
    });

    await check('рендер: «Сбросить» активен только для override, иначе disabled с пояснением', function () {
        const h = html();
        const resetBtn = function (name) {
            const i = h.indexOf('data-hot-action="reset" data-hot-name="' + name + '"');
            assert.ok(i >= 0, 'нет кнопки сброса у ' + name);
            return h.slice(i, h.indexOf('</button>', i));
        };
        assert.ok(resetBtn('LB_ADMISSION_WAIT_SEC').indexOf('disabled') === -1,
            'у override-строки сброс обязан быть доступен');
        assert.ok(resetBtn('LB_STREAM_STALL_ABORT_SEC').indexOf('disabled') >= 0,
            'у source=default сброс обязан быть disabled');
        assert.ok(resetBtn('LB_STREAM_STALL_ABORT_SEC').indexOf('Сбрасывать нечего') >= 0,
            'disabled без пояснения: оператор не поймёт, почему кнопка не работает');
    });

    await check('рендер: подсказка про env — env остаётся запасным значением', function () {
        const row = rowHtml(html(), 'LB_DONOR_ALLOW_OWN_COPIES');
        assert.ok(row.indexOf('env остаётся как запасное значение') >= 0,
            'у source=env нет подсказки, что после сброса вернётся env');
        assert.ok(row.indexOf('по умолчанию: 0') >= 0,
            'у env-строки не видно, какое значение вернётся после сброса');
    });

    await check('рендер: поле ввода по типу — select 0/1 для bool, number для duration_sec', function () {
        const boolRow = rowHtml(html(), 'LB_DONOR_ALLOW_OWN_COPIES');
        assert.ok(/<select[^>]*data-hot-input="LB_DONOR_ALLOW_OWN_COPIES"/.test(boolRow),
            'для bool ожидался select 0/1');
        assert.ok(boolRow.indexOf('<option value="0"') >= 0 && boolRow.indexOf('<option value="1"') >= 0);
        const secRow = rowHtml(html(), 'LB_ADMISSION_WAIT_SEC');
        assert.ok(/<input type="number"[^>]*data-hot-input="LB_ADMISSION_WAIT_SEC"/.test(secRow),
            'для duration_sec ожидалось числовое поле');
        assert.ok(secRow.indexOf('data-admin-only') >= 0, 'админские контролы не помечены data-admin-only');
    });

    // --- 3. изменение: PUT с числом и обновление строки из ответа -----------
    await check('«Применить» (duration_sec): PUT {"name","value":45} и строка из ОТВЕТА', async function () {
        calls.length = 0;
        routes['PUT /api/v1/settings'] = function (body) {
            return {
                status: 200,
                body: {
                    status: 'updated', name: body.name, value: body.value,
                    effective: body.value, source: 'override', applied: 'override', hotReload: true,
                },
            };
        };
        const res = await SR.apply('LB_ADMISSION_WAIT_SEC', '45');
        assert.strictEqual(res.ok, true, 'apply() не подтвердил успех');
        assert.strictEqual(puts().length, 1, 'ожидался ровно один PUT /api/v1/settings');
        assert.deepStrictEqual(puts()[0].body, { name: 'LB_ADMISSION_WAIT_SEC', value: 45 },
            'в PUT ушло не {"name","value":<число>}: ' + JSON.stringify(puts()[0].body));
        assert.strictEqual(typeof puts()[0].body.value, 'number', 'value обязан быть числом, а не строкой');
        const row = rowHtml(html(), 'LB_ADMISSION_WAIT_SEC');
        assert.ok(row.indexOf('45 сек') >= 0, 'строка не обновилась из ответа (нет нового effective): ' + row.slice(0, 300));
        assert.ok(row.indexOf('задано в панели') >= 0, 'после PUT источник обязан стать override');
        assert.ok(row.indexOf('применено') >= 0, 'оператор не видит подтверждения применения');
        assert.strictEqual(gets().length, 0, 'строка обновляется из ответа, без лишнего GET');
    });

    await check('«Применить» (bool): value=0 числом, строка показывает «0 (выкл)»', async function () {
        calls.length = 0;
        const res = await SR.apply('LB_DONOR_ALLOW_OWN_COPIES', '0');
        assert.strictEqual(res.ok, true);
        assert.deepStrictEqual(puts()[0].body, { name: 'LB_DONOR_ALLOW_OWN_COPIES', value: 0 },
            'bool обязан уходить как 0/1, а не true/false');
        const row = rowHtml(html(), 'LB_DONOR_ALLOW_OWN_COPIES');
        assert.ok(row.indexOf('0 (выкл)') >= 0, 'bool effective не обновился: ' + row.slice(0, 300));
        assert.ok(row.indexOf('задано в панели') >= 0);
    });

    await check('«Применить» читает набранное в поле строки (путь браузера, без аргумента)', async function () {
        calls.length = 0;
        inputValues['LB_STREAM_STALL_ABORT_SEC'] = '90';
        await SR.apply('LB_STREAM_STALL_ABORT_SEC');
        delete inputValues['LB_STREAM_STALL_ABORT_SEC'];
        assert.strictEqual(puts().length, 1, 'набранное в поле значение не ушло в PUT');
        assert.strictEqual(puts()[0].body.value, 90, 'в PUT ушло не то значение: ' + JSON.stringify(puts()[0].body));
    });

    await check('пока ответа нет, «Применить»/«Сбросить» строки погашены (нет второго PUT)', async function () {
        calls.length = 0;
        let release = null;
        routes['PUT /api/v1/settings'] = function () {
            return new Promise(function (resolve) { release = resolve; });
        };
        const pending = SR.apply('LB_ADMISSION_WAIT_SEC', '77');
        const row = rowHtml(html(), 'LB_ADMISSION_WAIT_SEC');
        assert.ok(/data-hot-action="apply"[^>]*disabled/.test(row),
            'кнопка «Применить» активна во время запроса — двойной клик пошлёт второй PUT');
        assert.ok(row.indexOf('применяю') >= 0, 'оператор не видит, что запрос уже ушёл');
        release({
            status: 200,
            body: { status: 'updated', name: 'LB_ADMISSION_WAIT_SEC', value: 77, effective: 77, source: 'override' },
        });
        const res = await pending;
        assert.strictEqual(res.ok, true);
        assert.strictEqual(puts().length, 1, 'ожидался один PUT');
        assert.strictEqual(rowHtml(html(), 'LB_ADMISSION_WAIT_SEC').indexOf('77 сек') >= 0, true);
    });

    // --- 4. сброс: POST reset {"name"} и перечитывание реестра --------------
    await check('«Сбросить»: POST /api/v1/settings/reset {"name"} → строка из следующего GET', async function () {
        calls.length = 0;
        const after = fixture();
        after.settings[2].effective = 300;
        after.settings[2].source = 'env';
        routes['POST /api/v1/settings/reset'] = function (body) {
            return { status: 200, body: { status: 'reset', name: body.name, effective: 300, source: 'env' } };
        };
        routes['GET /api/v1/settings'] = { status: 200, body: after };
        const res = await SR.reset('LB_ADMISSION_WAIT_SEC');
        assert.strictEqual(res.ok, true);
        assert.strictEqual(resets().length, 1, 'ожидался ровно один POST /api/v1/settings/reset');
        assert.deepStrictEqual(resets()[0].body, { name: 'LB_ADMISSION_WAIT_SEC' },
            'reset ушёл не с {"name"}: ' + JSON.stringify(resets()[0].body));
        assert.strictEqual(puts().length, 0, 'сброс не должен ничего записывать через PUT');
        // Порядок важен: сначала POST, потом GET (значение после сброса знает только сервер).
        const order = calls.filter(function (c) { return c.path.indexOf('/api/v1/settings') === 0; })
            .map(function (c) { return c.method; });
        assert.deepStrictEqual(order, ['POST', 'GET'],
            'после reset панель обязана перечитать реестр GET-ом: ' + order.join(','));
        const row = rowHtml(html(), 'LB_ADMISSION_WAIT_SEC');
        assert.ok(row.indexOf('300 сек') >= 0, 'строка не обновилась из GET после сброса: ' + row.slice(0, 300));
        assert.ok(row.indexOf('из env') >= 0, 'после сброса источник обязан стать env');
        assert.ok(row.indexOf('сброшено') >= 0,
            'оператор не видит подтверждения сброса (сообщение обязано ставиться ПОСЛЕ перечитывания)');
    });

    await check('«Сбросить» у source!=override не шлёт запрос вовсе', async function () {
        calls.length = 0;
        const res = await SR.reset('LB_STREAM_STALL_ABORT_SEC');
        assert.strictEqual(res.ok, false);
        assert.strictEqual(res.reason, 'not-override');
        assert.strictEqual(resets().length, 0, 'сброс у не-override значения ушёл на сервер');
    });

    // --- 5. мусор: ни одного PUT, ошибка в строке ---------------------------
    await check('мусор "": PUT НЕ уходит, ошибка видна в строке', async function () {
        calls.length = 0;
        const res = await SR.apply('LB_ADMISSION_WAIT_SEC', '');
        assert.strictEqual(res.ok, false);
        assert.strictEqual(res.reason, 'invalid');
        assert.strictEqual(puts().length, 0, 'пустое значение ушло в PUT');
        const row = rowHtml(html(), 'LB_ADMISSION_WAIT_SEC');
        assert.ok(row.indexOf('значение не введено') >= 0, 'нет ошибки про пустое значение: ' + row.slice(0, 300));
        assert.ok(row.indexOf('is-error') >= 0, 'ошибка строки не помечена классом is-error');
    });

    await check('мусор "abc" и "30s": PUT НЕ уходит, ошибка «нужно целое число»', async function () {
        for (const bad of ['abc', '30s', '1.5']) {
            calls.length = 0;
            const res = await SR.apply('LB_ADMISSION_WAIT_SEC', bad);
            assert.strictEqual(res.ok, false, 'значение "' + bad + '" принято как валидное');
            assert.strictEqual(puts().length, 0, 'нечисло "' + bad + '" ушло в PUT');
            assert.ok(rowHtml(html(), 'LB_ADMISSION_WAIT_SEC').indexOf('нужно целое число') >= 0,
                'нет понятной ошибки для "' + bad + '"');
        }
    });

    await check('bool: значение 2 отклоняется ДО запроса', async function () {
        calls.length = 0;
        const res = await SR.apply('LB_DONOR_ALLOW_OWN_COPIES', '2');
        assert.strictEqual(res.ok, false);
        assert.strictEqual(puts().length, 0, 'для bool ушло 2 — сервер вернул бы общий 400');
        const c = SR.coerceValue('bool', '2');
        assert.strictEqual(c.ok, false);
        assert.strictEqual(SR.coerceValue('bool', '1').value, 1);
        assert.strictEqual(SR.coerceValue('duration_sec', ' 30 ').value, 30, 'пробелы по краям — не мусор');
    });

    // --- 6. старый балансер: честное сообщение, а не пустота ----------------
    await check('старый балансер (404): «реестр недоступен (старый балансер)»', async function () {
        routes = { 'GET /api/v1/settings': { status: 404, body: { error: '404 page not found' } } };
        let list = null;
        await assert.doesNotReject(async function () { list = await SR.load(); },
            '404 на /api/v1/settings уронил загрузку');
        assert.strictEqual(list, null);
        const h = html();
        assert.ok(h.indexOf('реестр недоступен (старый балансер)') >= 0,
            'нет честного сообщения про старый балансер: ' + h.slice(0, 400));
        assert.ok(h.indexOf('GET /api/v1/settings') >= 0, 'подсказка обязана называть эндпоинт');
        assert.strictEqual(h.indexOf('hot-setting-row'), -1, 'при недоступном реестре не должно быть строк');
    });

    await check('ответ без поля settings (нет реестра): тоже «реестр недоступен», не пустота', async function () {
        routes = { 'GET /api/v1/settings': { status: 200, body: { count: 0 } } };
        let list = null;
        await assert.doesNotReject(async function () { list = await SR.load(); });
        assert.strictEqual(list, null);
        assert.ok(html().indexOf('реестр недоступен (старый балансер)') >= 0,
            'ответ без settings принят за «настроек нет»: ' + html().slice(0, 400));
    });

    await check('честный пустой реестр: отдельный текст, а не «старый балансер»', async function () {
        routes = { 'GET /api/v1/settings': { status: 200, body: { count: 0, settings: [], sourcePriority: ['override', 'env', 'default'] } } };
        await SR.load();
        assert.ok(html().indexOf('Реестр настроек пуст') >= 0,
            'пустой массив settings обязан говорить «реестр пуст»: ' + html().slice(0, 400));
    });

    await check('сетевой сбой/500: ошибка загрузки с причиной, без падения', async function () {
        routes = { 'GET /api/v1/settings': { status: 500, body: { error: 'registry exploded' } } };
        await assert.doesNotReject(async function () { await SR.load(); });
        assert.ok(html().indexOf('Не удалось прочитать реестр настроек') >= 0,
            'нет сообщения об ошибке загрузки: ' + html().slice(0, 400));
    });

    // --- 7. i18n: ни одного сырого ключа в отрисованной панели --------------
    await check('все ключи панели переведены (ru.js/en.js), сырых ключей нет', async function () {
        routes = { 'GET /api/v1/settings': { status: 200, body: fixture() } };
        await SR.load();
        assert.deepStrictEqual(missingKeys, [],
            'ключи без перевода в ru.js/en.js: ' + missingKeys.join(', '));
        assert.strictEqual(html().indexOf('settings.hot.'), -1,
            'в разметке остался сырой i18n-ключ: ' + html().slice(0, 400));
        // Модуль обязан называть ключи литералами: динамический t('...'+group) линт
        // i18n_keys_lint_test.go не видит, и ключ может исчезнуть незаметно.
        assert.ok(MODULE_SRC.indexOf("t('settings.hot.group.streaming')") >= 0,
            'ключи групп не переданы в t() литералами — линт i18n их не проверит');
    });

    // --- 8. английский язык: подписи меняются без запроса -------------------
    await check('смена языка перерисовывает панель из уже загруженного состояния', async function () {
        calls.length = 0;
        currentLang = 'en';
        SR.render();
        const h = html();
        assert.ok(h.indexOf('set in the panel') >= 0, 'нет английского бейджа override: ' + h.slice(0, 300));
        assert.ok(h.indexOf('Context window') >= 0, 'нет английского заголовка группы nctx');
        assert.strictEqual(gets().length, 0, 'смена языка не должна ходить в сеть');
        currentLang = 'ru';
        SR.render();
    });

    // --- 9. R317: пометка «управляется в панели ролей» и якоря --------------
    await check('R317: строка донора помечена «управляется в панели ролей» и ведёт на «Монитор»', async function () {
        routes = { 'GET /api/v1/settings': { status: 200, body: fixture() } };
        await SR.load();
        const h = html();
        // Строка ОСТАЛАСЬ в общем списке (страница настроек — полный реестр):
        // перенос органа управления не должен прятать саму настройку.
        assert.ok(h.indexOf('id="hot-setting-LB_DONOR_ALLOW_OWN_COPIES"') >= 0,
            'строка донора исчезла из общего списка реестра');
        const row = rowHtml(h, 'LB_DONOR_ALLOW_OWN_COPIES');
        assert.ok(row.indexOf('data-hot-managed="LB_DONOR_ALLOW_OWN_COPIES"') >= 0,
            'нет пометки «управляется в панели ролей»: ' + row.slice(-300));
        assert.ok(row.indexOf('управляется в панели ролей') >= 0, 'пометка без текста');
        assert.ok(row.indexOf('data-open-monitor="1"') >= 0, 'пометка без ссылки на «Монитор»');
        assert.ok(row.indexOf('Открыть «Монитор»') >= 0, 'ссылка без подписи: ' + row.slice(-300));
        // У остальных строк такой пометки быть не должно: она значит «есть второй
        // орган управления», а не «эта строка важная».
        assert.strictEqual(rowHtml(h, 'LB_ADMISSION_WAIT_SEC').indexOf('data-hot-managed'), -1,
            'пометка «в панели ролей» появилась у посторонней настройки');
    });

    await check('R317: get()/set() — те же операции реестра для панели ролей', async function () {
        const before = SR.get('LB_DONOR_ALLOW_OWN_COPIES');
        assert.ok(before && typeof before.effective === 'number', 'get() не отдаёт запись реестра');
        assert.strictEqual(SR.get('LB_NO_SUCH_SETTING'), null, 'get() незнакомого имени обязан вернуть null');
        calls.length = 0;
        routes['PUT /api/v1/settings'] = function (body) {
            return {
                status: 200,
                body: { status: 'updated', name: body.name, value: body.value, effective: body.value, source: 'override' },
            };
        };
        const res = await SR.set('LB_DONOR_ALLOW_OWN_COPIES', 1);
        assert.strictEqual(res.ok, true, 'set() не подтвердил успех');
        assert.deepStrictEqual(puts()[0].body, { name: 'LB_DONOR_ALLOW_OWN_COPIES', value: 1 },
            'set() ушёл не тем же PUT, что «Применить»: ' + JSON.stringify(puts()[0].body));
        // Состояние одно: панель ролей читает ровно эту запись, копии не заводит.
        assert.strictEqual(SR.get('LB_DONOR_ALLOW_OWN_COPIES').source, 'override');
        assert.strictEqual(SR.get('LB_DONOR_ALLOW_OWN_COPIES').effective, 1);
    });

    await check('R317: viewer видит «Сбросить» погашенной с причиной про роль', async function () {
        // ЗАЧЕМ: клик по кнопке давал честный, но лишний 403 — сеть вместо
        // объяснения. Признак роли берём у auth.js (window.Auth.isAdmin()).
        global.Auth = { isAdmin: function () { return false; } };
        routes = { 'GET /api/v1/settings': { status: 200, body: fixture() } };
        await SR.load();
        let row = rowHtml(html(), 'LB_ADMISSION_WAIT_SEC');
        let i = row.indexOf('data-hot-action="reset"');
        let btn = row.slice(i, row.indexOf('</button>', i));
        assert.ok(btn.indexOf('disabled') >= 0, 'viewer видит активный сброс: ' + btn);
        assert.ok(btn.indexOf('только администратору') >= 0,
            'viewer не видит, ПОЧЕМУ кнопка погашена (роль, а не источник): ' + btn);
        assert.ok(row.indexOf('data-hot-admin="0"') >= 0, 'признак роли не отражён в разметке строки');

        global.Auth = { isAdmin: function () { return true; } };
        SR.render();
        row = rowHtml(html(), 'LB_ADMISSION_WAIT_SEC');
        i = row.indexOf('data-hot-action="reset"');
        btn = row.slice(i, row.indexOf('</button>', i));
        assert.ok(btn.indexOf('disabled') === -1, 'администратор не может сбросить override: ' + btn);
        delete global.Auth;
    });

    await check('R317: якорь из старого поля раскрывает секцию реестра и находит строку', function () {
        // Ссылка «перекрыто горячей настройкой» ведёт на #hot-setting-<имя>.
        // Секция — аккордеон: при свёрнутой ссылка вела бы в пустой заголовок.
        const added = [];
        const bodyEl = { style: {}, classList: { add: function (c) { added.push(c); } } };
        const toggleEl = { textContent: '▶' };
        DOM['section-hot-settings'] = {
            querySelector: function (sel) {
                if (sel === '.settings-section-body') { return bodyEl; }
                if (sel === '.section-toggle') { return toggleEl; }
                return null;
            },
        };
        let scrolled = 0;
        DOM['hot-setting-LB_PLACE_OVER_RELOAD'] = {
            style: {},
            scrollIntoView: function () { scrolled++; },
        };
        assert.strictEqual(SR.focusRow('LB_PLACE_OVER_RELOAD'), true, 'строка якоря не найдена');
        assert.ok(added.indexOf('open') >= 0, 'секция реестра не раскрыта по якорю');
        assert.strictEqual(toggleEl.textContent, '▼', 'стрелка секции не обновлена');
        assert.strictEqual(scrolled, 1, 'к строке не прокрутили');
        // У старого балансера строки нет — честный false вместо исключения.
        assert.strictEqual(SR.focusRow('LB_UNKNOWN_ROW'), false);
    });

    await check('R317: делегированный клик — якорь раскрывает строку, «Монитор» переключает страницу', function () {
        // Ссылки стоят в СТАТИЧЕСКОЙ разметке (рядом со старыми полями), поэтому
        // обработчик живёт на документе. Без него href="#" прыгал бы к пустому id.
        const click = domListeners['click'];
        assert.ok(typeof click === 'function', 'клик по якорям реестра не делегирован документу');
        let prevented = 0;
        click({
            preventDefault: function () { prevented++; },
            target: {
                closest: function (sel) {
                    return sel === '[data-hot-goto]'
                        ? { getAttribute: function () { return 'LB_PLACE_OVER_RELOAD'; } }
                        : null;
                },
            },
        });
        assert.strictEqual(prevented, 1, 'переход по hash не отменён — браузер прыгнет к пустому месту');

        const savedQS = global.document.querySelector;
        let navClicks = 0;
        global.document.querySelector = function () { return { click: function () { navClicks++; } }; };
        let prevented2 = 0;
        try {
            click({
                preventDefault: function () { prevented2++; },
                target: {
                    closest: function (sel) { return sel === '[data-open-monitor]' ? {} : null; },
                },
            });
        } finally {
            global.document.querySelector = savedQS;
        }
        assert.strictEqual(navClicks, 1, 'кнопка «Открыть «Монитор»» не переключает страницу');
        assert.strictEqual(prevented2, 1, 'клик по кнопке «Монитор» не гасит default-поведение');
    });

    console.log('\nsettings-registry: ' + passed + ' проверок пройдено' +
        (failures.length ? ', ПРОВАЛОВ: ' + failures.length : ''));
    if (failures.length) { process.exit(1); }
})().catch(function (e) {
    console.error('settings-registry.test.js упал вне проверки:', e);
    process.exit(1);
});
