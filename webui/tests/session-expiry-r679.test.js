// webui/tests/session-expiry-r679.test.js — R679 (2026-11-21): ИСТЁКШАЯ СЕССИЯ.
//
// ЖИВАЯ ЖАЛОБА (дословно): «Сейчас сыпятся ошибки что пользователь неавторизован,
// и только перезагрузка открывает меню входа, но при этом режим просмотра не
// активен — то есть не переходит на пользователя гостя, что может только
// смотреть некоторую информацию».
//
// ЧТО ПРОВЕРЯЕТСЯ (контракт, а не «выглядит красиво»). Модуль загружается в node
// с минимальным DOM, 401 приходит РЕАЛЬНЫМ путём (api.js request() и
// websocket.js handshake), а проверяется видимое состояние панели:
//
//   (а) тело получает класс auth-role-guest, auth-role-admin снят;
//   (б) [data-admin-only] скрыты и disabled (Auth.applyRoleVisibility);
//   (в) показано РОВНО ОДНО объяснение — полоса #authBanner ИЛИ слой #authOverlay,
//       и его текст говорит «сессия завершилась»;
//   (г) второй и третий 401 подряд не добавляют ни тостов, ни новых объяснений
//       (счётчик подавленных уходит ОДНОЙ строкой в console.info на окно);
//   (д) после «входа» (успешный applyMe/auth.me) админские действия возвращаются
//       и класс роли снова admin — без перезагрузки страницы (в инициализированном
//       приложении reload не вызывается вообще);
//   (е) 401 от /api/v1/auth/login (неверный пароль) НЕ разлогинивает панель;
//   (ж) 403 не считается истечением сессии (это роль/CSRF);
//   (з) обычные ошибки (HTTP 500) по-прежнему показываются — шум не прячет реальные
//       проблемы.
//
// ПОЧЕМУ 401 ПРИХОДИТ ЧЕРЕЗ api.js/websocket.js, А НЕ ВЫЗОВОМ SessionExpiry.note:
// иначе тест проверял бы сам себя. Живой дефект был в том, что 401 НЕ доходил до
// единой точки — важно, что транспорт его передаёт.
//
// Запуск: node webui/tests/session-expiry-r679.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const ROOT = path.resolve(__dirname, '..', '..');
const read = (rel) => fs.readFileSync(path.join(ROOT, rel), 'utf8');

let passed = 0;
const failures = [];
function check(name, fn) {
    return Promise.resolve().then(fn).then(function () {
        passed++;
        console.log('  \u2713 ' + name);
    }, function (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' — ' + (e && e.message));
    });
}

// ===========================================================================
// Минимальный DOM
// ===========================================================================
//
// Достаточно честный, чтобы auth.js мог создавать слой/полосу/чип, а тест —
// видеть, СКОЛЬКО объяснений показано (в этом суть пункта «в»).

const elements = {};

function makeClassList() {
    const set = new Set();
    return {
        add: function (c) { set.add(c); },
        remove: function (c) { set.delete(c); },
        contains: function (c) { return set.has(c); },
        toggle: function (c, on) {
            if (on === undefined) { set.has(c) ? set.delete(c) : set.add(c); }
            else if (on) { set.add(c); } else { set.delete(c); }
        },
        _all: function () { return Array.from(set); }
    };
}

function makeEl(id) {
    const attrs = {};
    const children = [];
    const el = {
        id: id || '',
        tagName: 'DIV',
        innerHTML: '',
        textContent: '',
        value: '',
        className: '',
        style: {},
        dataset: {},
        disabled: false,
        children: children,
        parentNode: null,
        classList: makeClassList(),
        _listeners: {},
        addEventListener: function (ev, fn) { (this._listeners[ev] = this._listeners[ev] || []).push(fn); },
        removeEventListener: function (ev, fn) {
            const list = this._listeners[ev] || [];
            const i = list.indexOf(fn);
            if (i >= 0) { list.splice(i, 1); }
        },
        dispatch: function (ev, payload) {
            (this._listeners[ev] || []).forEach(function (fn) { fn(payload || {}); });
        },
        appendChild: function (child) {
            child.parentNode = this;
            children.push(child);
            if (child.id) { elements[child.id] = child; }
            return child;
        },
        removeChild: function (child) {
            const i = children.indexOf(child);
            if (i >= 0) { children.splice(i, 1); }
            child.parentNode = null;
            if (child.id && elements[child.id] === child) { delete elements[child.id]; }
            return child;
        },
        remove: function () { if (this.parentNode) { this.parentNode.removeChild(this); } },
        setAttribute: function (n, v) { attrs[n] = String(v); },
        getAttribute: function (n) { return Object.prototype.hasOwnProperty.call(attrs, n) ? attrs[n] : null; },
        hasAttribute: function (n) { return Object.prototype.hasOwnProperty.call(attrs, n); },
        removeAttribute: function (n) { delete attrs[n]; },
        focus: function () {},
        contains: function (other) { return other === el; },
        querySelectorAll: function () { return []; },
        querySelector: function () { return null; }
    };
    if (id) { elements[id] = el; }
    return el;
}

/** adminOnly — узлы разметки, помеченные data-admin-only (на них смотрит роль). */
const adminOnly = [];

const documentListeners = {};
global.document = {
    readyState: 'complete',
    activeElement: null,
    _listeners: documentListeners,
    body: makeEl('__body'),
    documentElement: { getAttribute: function () { return 'dark'; }, setAttribute: function () {} },
    getElementById: function (id) { return elements[id] || null; },
    createElement: function () { return makeEl(''); },
    addEventListener: function (ev, fn) { (documentListeners[ev] = documentListeners[ev] || []).push(fn); },
    querySelectorAll: function (sel) { return sel === '[data-admin-only]' ? adminOnly : []; },
    querySelector: function () { return null; }
};
global.document.body.classList = makeClassList();

const sessionStore = {};
global.sessionStorage = {
    getItem: function (k) { return Object.prototype.hasOwnProperty.call(sessionStore, k) ? sessionStore[k] : null; },
    setItem: function (k, v) { sessionStore[k] = String(v); },
    removeItem: function (k) { delete sessionStore[k]; }
};
const localStore = {};
global.localStorage = {
    getItem: function (k) { return Object.prototype.hasOwnProperty.call(localStore, k) ? localStore[k] : null; },
    setItem: function (k, v) { localStore[k] = String(v); },
    removeItem: function (k) { delete localStore[k]; }
};

// window = global: модули WebUI — classic-script'ы, они пишут в window.
global.window = global;
global.WEBUI_CONFIG = {};
global.location = {
    reloads: 0,
    reload: function () { this.reloads++; },
    origin: 'http://127.0.0.1:18083',
    protocol: 'http:',
    host: '127.0.0.1:18083',
    href: 'http://127.0.0.1:18083/',
    search: '',
    hash: ''
};

// --- шина событий window (CustomEvent-ы WebUI) ------------------------------
const windowListeners = {};
global.window.addEventListener = function (ev, fn) {
    (windowListeners[ev] = windowListeners[ev] || []).push(fn);
};
global.window.removeEventListener = function () {};
global.window.dispatchEvent = function (ev) {
    (windowListeners[ev.type] || []).forEach(function (fn) {
        try { fn(ev); } catch (e) { /* подписчик не должен ломать тест */ }
    });
    return true;
};
global.CustomEvent = function (type, init) {
    this.type = type;
    this.detail = (init && init.detail) || null;
};

// --- тосты -----------------------------------------------------------------
const toasts = [];
const toastContainer = makeEl('toastContainer');
global.Utils = {
    escapeHtml: function (s) { return String(s); },
    setText: function (id, v) {
        const el = global.document.getElementById(id);
        if (el) { el.textContent = v; }
    }
};

// --- консоль ---------------------------------------------------------------
const consoleInfo = [];
const consoleErrors = [];
const realInfo = console.info;
const realError = console.error;
console.info = function () { consoleInfo.push(Array.from(arguments).join(' ')); };
console.error = function () { consoleErrors.push(Array.from(arguments).map(String).join(' ')); };

// ===========================================================================
// Управляемый fetch
// ===========================================================================
//
// route: набор правил «подстрока URL → ответ». 401 приходит ИЗ ТРАНСПОРТА, как
// на стенде: тест не вызывает note() напрямую.

let routes = [];
let fetchLog = [];

function makeResponse(rule) {
    const bodyText = typeof rule.body === 'string' ? rule.body : JSON.stringify(rule.body || {});
    return {
        ok: rule.status >= 200 && rule.status < 300,
        status: rule.status,
        statusText: rule.statusText || (rule.status === 401 ? 'Unauthorized' : (rule.status === 403 ? 'Forbidden' : 'Error')),
        headers: { get: function (n) { return String(n).toLowerCase() === 'content-type' ? 'application/json' : null; } },
        json: function () { return Promise.resolve(typeof rule.body === 'string' ? JSON.parse(rule.body) : (rule.body || {})); },
        text: function () { return Promise.resolve(bodyText); }
    };
}

global.fetch = function (url, opts) {
    const u = String(url);
    fetchLog.push({ url: u, opts: opts || {} });
    for (let i = 0; i < routes.length; i++) {
        if (u.indexOf(routes[i].match) >= 0) {
            const rule = routes[i];
            if (rule.never === true) {
                // «висит»: используется только для проверки, что запрос был сделан
                return new Promise(function () {});
            }
            const res = makeResponse(rule);
            if (rule.delay) {
                return new Promise(function (resolve) { setTimeout(function () { resolve(res); }, rule.delay); });
            }
            return Promise.resolve(res);
        }
    }
    return Promise.resolve(makeResponse({ status: 200, body: {} }));
};

// ===========================================================================
// Загрузка модулей WebUI (classic-script'ы)
// ===========================================================================

function loadModule(rel) {
    const src = read(rel);
    // new Function — как <script>: модуль выполняется в текущем контексте и сам
    // пишет себя в window (auth.js → window.Auth, session-expiry.js → window.SessionExpiry).
    new Function(src)();
}

function resetEnv() {
    Object.keys(elements).forEach(function (k) { delete elements[k]; });
    global.document.body.children.length = 0;
    elements['__body'] = global.document.body;
    elements['toastContainer'] = toastContainer;
    toastContainer.children.length = 0;
    adminOnly.length = 0;
    toasts.length = 0;
    fetchLog = [];
    routes = [];
    consoleInfo.length = 0;
    consoleErrors.length = 0;
    Object.keys(windowListeners).forEach(function (k) { delete windowListeners[k]; });
    Object.keys(sessionStore).forEach(function (k) { delete sessionStore[k]; });
    global.document.body.classList = makeClassList();
    global.location.reloads = 0;
    // Сброс кэша «загруженных» модулей: каждый сценарий начинает с чистого Auth.
    delete global.Auth;
    delete global.SessionExpiry;
    delete global.Api;
    delete global.WebSocketManager;
    delete global.DataRefresh;
    delete global.__olAppInitialized;
}

/** loadApp — поднять auth.js + session-expiry.js + api.js (без app.js). */
function loadCore() {
    loadModule('webui/js/modules/auth.js');
    loadModule('webui/js/modules/session-expiry.js');
    loadModule('webui/js/modules/api.js');
}

/** meAdmin — ответ /auth/me для вошедшего администратора. */
function meAdmin() {
    return { match: '/api/v1/auth/me', status: 200, body: { authenticated: true, usersConfigured: true, user: { name: 'skyadmin', role: 'admin' }, csrf: 'csrf-679' } };
}

function adminNode(id) {
    const el = makeEl(id || ('adminBtn' + adminOnly.length));
    el.setAttribute('data-admin-only', '1');
    el.style.display = '';
    adminOnly.push(el);
    return el;
}

/** explanationCount — сколько РАЗНЫХ объяснений сейчас видно (полоса + слой). */
function explanationCount() {
    let n = 0;
    const bar = elements['authBanner'];
    if (bar && bar.style.display !== 'none') { n++; }
    const overlay = elements['authOverlay'];
    if (overlay && overlay.style.display === 'flex') { n++; }
    return n;
}

function bannerText() {
    const bar = elements['authBanner'];
    if (!bar) { return ''; }
    return bar.children.map(function (c) { return c.textContent || ''; }).join(' ');
}

function overlayVisible() {
    const ov = elements['authOverlay'];
    return !!(ov && ov.style.display === 'flex');
}

function fetchCount(match) {
    return fetchLog.filter(function (f) { return f.url.indexOf(match) >= 0; }).length;
}

const sleep = (ms) => new Promise(function (r) { setTimeout(r, ms); });

// ===========================================================================
// Сценарии
// ===========================================================================
(async function main() {
    console.log('session-expiry-r679.test.js — R679\n');

    // -----------------------------------------------------------------------
    await check('(а,б,в) 401 от API: гость, админские действия скрыты, РОВНО одно объяснение', async function () {
        resetEnv();
        const adminBtn = adminNode('addBackendBtn');
        routes = [meAdmin(), { match: '/api/v1/cluster', status: 401, body: { error: 'Unauthorized: valid API token required' } }];
        loadCore();
        await window.__authReady();

        // До аварии: админ, кнопка видима, класс роли admin.
        assert.strictEqual(window.Auth.isAdmin(), true, 'сценарий обязан начинаться вошедшим админом');
        assert.strictEqual(global.document.body.classList.contains('auth-role-admin'), true, 'класс админа не выставлен');
        assert.notStrictEqual(adminBtn.style.display, 'none', 'админская кнопка обязана быть видима у админа');

        // Живой путь: модуль запрашивает данные, сервер отвечает 401.
        let failed = null;
        try { await window.Api.cluster(); } catch (e) { failed = e; }
        assert.ok(failed, 'запрос обязан был упасть на 401');
        assert.strictEqual(failed.status, 401, 'статус ошибки должен сохраниться');

        // (а) состояние → гость, класс роли переставлен (admin снят!).
        const st = window.Auth.state();
        assert.strictEqual(st.authenticated, false, 'state.authenticated должен стать false');
        assert.strictEqual(st.user, null, 'state.user должен стать null');
        assert.strictEqual(st.csrf, '', 'state.csrf должен стать пустым');
        assert.strictEqual(st.tokenAuth, false, 'state.tokenAuth должен стать false');
        assert.strictEqual(global.document.body.classList.contains('auth-role-guest'), true,
            'тело обязано получить auth-role-guest');
        assert.strictEqual(global.document.body.classList.contains('auth-role-admin'), false,
            'auth-role-admin обязан быть снят (иначе панель в двух ролях одновременно)');

        // (б) админские действия скрыты/отключены существующим механизмом.
        assert.strictEqual(adminBtn.style.display, 'none', '[data-admin-only] обязан быть скрыт');
        assert.strictEqual(adminBtn.getAttribute('disabled'), 'disabled', '[data-admin-only] обязан быть disabled');
        assert.strictEqual(window.Auth.isAdmin(), false, 'гость не считается администратором');

        // (в) РОВНО ОДНО объяснение «сессия завершилась».
        assert.strictEqual(explanationCount(), 1,
            'объяснение должно быть одно, а не полоса+слой; видно: ' + explanationCount());
        assert.ok(overlayVisible() || /Сессия завершилась/i.test(bannerText()),
            'текст объяснения должен говорить, что сессия завершилась');
        assert.strictEqual(global.location.reloads, 0,
            'перезагрузка для перехода в гостя НЕ нужна (в этом и была жалоба)');
    });

    // -----------------------------------------------------------------------
    await check('(в, R667) при сохранённом ol_auth_guest_ack — только полоса, слой не перекрывает', async function () {
        resetEnv();
        // R683 (2026-11-22): ПРЕДУСЛОВИЕ СЦЕНАРИЯ, а не послабление проверки.
        //
        // Правило «флажок R667 переводит объяснение из слоя в полосу» относится к
        // РАБОТАЮЩЕЙ панели: данные уже на экране, оператору достаточно полосы.
        // У неинициализированной гостевой страницы данных нет вовсе, и полоса —
        // единственное объяснение — превращала её в мёртвый скелет без пути внутрь
        // (живой дефект r682: «повис, не прогружая страницу»). Поэтому auth.js
        // решает «полоса или слой» по window.__olAppInitialized
        // (canExplainWithBanner, R683), а здесь проверяется именно работающая
        // панель — ставим её флаг, как в жизни на момент истечения сессии.
        global.__olAppInitialized = true;
        adminNode('adminBtnAck');
        routes = [meAdmin(), { match: '/api/v1/cluster', status: 401, body: {} }];
        loadCore();
        await window.__authReady();
        // R667-флажок ставится ПОСЛЕ входа: успешный вход его гасит (это правило
        // R667 и оно остаётся), а сценарий проверяет именно случай «оператор уже
        // отказывался входить в этой вкладке» на момент истечения сессии.
        sessionStore['ol_auth_guest_ack'] = '1';
        try { await window.Api.cluster(); } catch (e) { /* 401 — ожидаем */ }

        assert.strictEqual(overlayVisible(), false,
            'оператор уже отказывался входить в этой вкладке (R667) — слой не должен перекрывать страницу');
        assert.ok(elements['authBanner'], 'обязана быть полоса с объяснением и кнопкой входа');
        assert.strictEqual(explanationCount(), 1, 'объяснение всё равно ровно одно');
        assert.ok(/Сессия завершилась/i.test(bannerText()), 'полоса должна говорить о завершении сессии');
    });

    // -----------------------------------------------------------------------
    await check('(г) второй и третий 401 не добавляют ни тостов, ни объяснений (счётчик — одной строкой)', async function () {
        resetEnv();
        adminNode('adminBtnNoise');
        routes = [meAdmin(), { match: '/', status: 401, body: { error: 'Unauthorized: valid API token required' } }];
        loadCore();
        await window.__authReady();

        // Первый 401 — переход и ОДНО объяснение.
        try { await window.Api.cluster(); } catch (e) { /* ожидаем */ }
        const afterFirst = {
            toasts: toasts.length,
            explanations: explanationCount(),
            consoleInfo: consoleInfo.length
        };
        assert.strictEqual(afterFirst.explanations, 1, 'после первого 401 обязано быть одно объяснение');

        // Второй и третий — тем же живым путём.
        try { await window.Api.cluster(); } catch (e) { /* ожидаем */ }
        try { await window.Api.queueStats(); } catch (e) { /* ожидаем */ }

        assert.strictEqual(explanationCount(), afterFirst.explanations,
            'повторные 401 не должны показывать второе объяснение');
        assert.strictEqual(toasts.length, afterFirst.toasts,
            'повторные 401 не должны порождать новые тосты (было ' + afterFirst.toasts + ', стало ' + toasts.length + ')');
        // Счётчик подавленных — ОДНА строка на окно, а не по строке на ошибку.
        const stats = window.SessionExpiry.stats();
        assert.ok(stats.suppressed >= 2,
            'счётчик подавленных должен расти (подавлено ' + stats.suppressed + ')');
        assert.ok(stats.totalSuppressed >= 2, 'общий счётчик подавленных тоже должен расти');

        // Console.info за окно — не больше одной «сводки о подавленных».
        const suppressedLines = consoleInfo.filter(function (l) { return /подавлено повторных/i.test(l); });
        assert.ok(suppressedLines.length <= 1,
            'сводка о подавленных должна быть одной строкой на окно, найдено ' + suppressedLines.length);
    });

    // -----------------------------------------------------------------------
    await check('(г) 30 секунд: повторный 401 внутри окна тишины не поднимает UI заново', async function () {
        resetEnv();
        adminNode('adminBtnWindow');
        routes = [meAdmin(), { match: '/', status: 401, body: {} }];
        loadCore();
        await window.__authReady();
        try { await window.Api.cluster(); } catch (e) { /* ожидаем */ }
        const before = { explanations: explanationCount(), toasts: toasts.length };
        for (let i = 0; i < 5; i++) {
            try { await window.Api.cluster(); } catch (e) { /* ожидаем */ }
        }
        assert.strictEqual(explanationCount(), before.explanations, 'пять повторов не должны дать второе объяснение');
        assert.strictEqual(toasts.length, before.toasts, 'пять повторов не должны дать тосты');
        assert.strictEqual(overlayVisible() || !!elements['authBanner'], true, 'объяснение остаётся на месте');
        assert.strictEqual(window.SessionExpiry.quietMs, 30000, 'окно подавления — 30 секунд');
    });

    // -----------------------------------------------------------------------
    await check('(д) явный вход возвращает админский режим БЕЗ перезагрузки', async function () {
        resetEnv();
        const adminBtn = adminNode('addBackendBtnRestore');
        routes = [
            meAdmin(),
            { match: '/api/v1/cluster', status: 401, body: {} },
            { match: '/api/v1/auth/login', status: 200, body: { success: true, user: { name: 'skyadmin', role: 'admin' }, csrf: 'csrf-restored' } }
        ];
        loadCore();
        await window.__authReady();
        // Приложение считаем инициализированным: app.js ставит этот флаг в init().
        global.__olAppInitialized = true;
        let resumed = 0;
        window.App = { resumeAfterAuth: function () { resumed++; return true; } };

        try { await window.Api.cluster(); } catch (e) { /* ожидаем 401 */ }
        assert.strictEqual(adminBtn.style.display, 'none', 'до входа кнопка скрыта');

        // Явный вход — как оператор: форма слоя (submit).
        const overlay = elements['authOverlay'];
        assert.ok(overlay, 'слой входа должен существовать после истечения сессии');
        elements['authName'] = elements['authName'] || makeEl('authName');
        elements['authPassword'] = elements['authPassword'] || makeEl('authPassword');
        elements['authName'].value = 'skyadmin';
        elements['authPassword'].value = 'secret';
        const submit = (overlay._listeners.submit || [])[0];
        assert.ok(submit, 'у слоя входа обязан быть обработчик submit');
        await submit({ preventDefault: function () {} });
        await sleep(0);

        assert.strictEqual(window.Auth.state().authenticated, true, 'вход обязан вернуть authenticated:true');
        assert.strictEqual(window.Auth.isAdmin(), true, 'вернулся администратор');
        assert.strictEqual(global.document.body.classList.contains('auth-role-admin'), true,
            'класс роли должен снова стать admin');
        assert.strictEqual(global.document.body.classList.contains('auth-role-guest'), false,
            'гостевой класс обязан быть снят');
        assert.notStrictEqual(adminBtn.style.display, 'none', 'админские действия обязаны вернуться');
        assert.strictEqual(explanationCount(), 0, 'объяснений больше не должно быть видно');
        assert.strictEqual(resumed, 1, 'приложение обязано получить сигнал «подними каналы и данные»');
        assert.strictEqual(global.location.reloads, 0,
            'в инициализированном приложении перезагрузки быть не должно');
        assert.strictEqual(window.SessionExpiry.isExpired(), false, 'флаг «сессия истекла» обязан сброситься');
    });

    // -----------------------------------------------------------------------
    await check('(д) гостевая загрузка: вход уместно перезагружает страницу (единственное место)', async function () {
        resetEnv();
        // __olAppInitialized НЕ выставлен: страница открыта гостем, boot-гейт
        // app.js не вызывал init(), восстанавливать нечего.
        adminNode('adminBtnGuestBoot');
        routes = [
            { match: '/api/v1/auth/me', status: 200, body: { authenticated: false, usersConfigured: true } },
            { match: '/api/v1/auth/login', status: 200, body: { success: true, user: { name: 'skyadmin', role: 'admin' }, csrf: 'c' } }
        ];
        loadCore();
        await window.__authReady();
        const overlay = elements['authOverlay'];
        assert.ok(overlay && overlay.style.display === 'flex', 'гостю показывается слой входа');
        elements['authName'] = elements['authName'] || makeEl('authName');
        elements['authPassword'] = elements['authPassword'] || makeEl('authPassword');
        elements['authName'].value = 'skyadmin';
        elements['authPassword'].value = 'secret';
        await (overlay._listeners.submit || [])[0]({ preventDefault: function () {} });
        await sleep(0);
        assert.ok(global.location.reloads >= 1,
            'на странице, открытой гостем, вход обязан перезагрузить страницу (init() не выполнялся)');
    });

    // -----------------------------------------------------------------------
    await check('(е) 401 от /api/v1/auth/login — это неверный пароль, а не истёкшая сессия', async function () {
        resetEnv();
        adminNode('adminBtnLogin401');
        routes = [meAdmin(), { match: '/api/v1/auth/login', status: 401, body: { error: 'invalid credentials' } }];
        loadCore();
        await window.__authReady();
        assert.strictEqual(window.Auth.isAdmin(), true, 'сценарий начинается админом');
        // Прямой запрос к транспорту: именно так уходит неверный пароль.
        let failed = null;
        try { await window.Api.post('/api/v1/auth/login', { name: 'x', password: 'y' }); } catch (e) { failed = e; }
        assert.ok(failed, 'логин обязан упасть');
        assert.strictEqual(window.SessionExpiry.isExpired(), false,
            'неверный пароль НЕ должен переводить панель в гостя');
        assert.strictEqual(window.Auth.isAdmin(), true, 'админ остаётся админом после опечатки в пароле');
    });

    // -----------------------------------------------------------------------
    await check('(ж) 403 — не истечение сессии (роль/CSRF), панель в гостя не переводится', async function () {
        resetEnv();
        adminNode('adminBtn403');
        routes = [meAdmin(), { match: '/api/v1/settings', status: 403, body: { error: 'admin required' } }];
        loadCore();
        await window.__authReady();
        let failed = null;
        try { await window.Api.hotSettings.get(); } catch (e) { failed = e; }
        assert.ok(failed, 'запрос обязан упасть');
        assert.strictEqual(window.SessionExpiry.isExpired(), false, '403 не означает «сессия истекла»');
        assert.strictEqual(window.Auth.isAdmin(), true, 'админ остаётся админом');
    });

    // -----------------------------------------------------------------------
    await check('(з) шум не прячет реальные ошибки: HTTP 500 по-прежнему показывает ошибку', async function () {
        resetEnv();
        adminNode('adminBtn500');
        routes = [meAdmin(), { match: '/api/v1/cluster', status: 500, body: { error: 'boom' } }];
        loadCore();
        await window.__authReady();
        try { await window.Api.cluster(); } catch (e) { /* ожидаем 500 */ }
        // handleError зовётся модулями (app-settings.js) — проверяем его поведение.
        window.Api.handleError({ status: 500, message: 'HTTP 500: Internal Server Error' }, 'Ошибка');
        assert.strictEqual(window.SessionExpiry.isExpired(), false, '500 — не истечение сессии');
        assert.strictEqual(consoleErrors.length >= 1, true, 'реальная ошибка обязана попасть в console.error');
    });

    // -----------------------------------------------------------------------
    await check('(в, г) отказ рукопожатия WebSocket проверяется через /auth/me и переводит в гостя', async function () {
        resetEnv();
        adminNode('adminBtnWs');
        let meCalls = 0;
        routes = [
            { match: '/api/v1/auth/me', status: 200, body: { authenticated: true, usersConfigured: true, user: { name: 'skyadmin', role: 'admin' }, csrf: 'c' } }
        ];
        loadCore();
        await window.__authReady();
        assert.strictEqual(window.Auth.isAdmin(), true, 'сценарий начинается админом');

        // Сессия «протухла»: сервер теперь отвечает authenticated:false (это и есть
        // ответ AuthorizeWS на рукопожатии — снаружи он невидим, см. probe()).
        routes = [{
            match: '/api/v1/auth/me', status: 200,
            body: { authenticated: false, usersConfigured: true }
        }];

        // Fake WebSocket: рукопожатие не открывается (401 отдал бы ровно это —
        // onerror без open, затем onclose с code 1006).
        const wsInstances = [];
        global.WebSocket = function (url) {
            this.url = url;
            this.readyState = 0;
            this.close = function () {};
            wsInstances.push(this);
        };
        global.WebSocket.OPEN = 1;
        loadModule('webui/js/modules/websocket.js');
        const before = fetchCount('/api/v1/auth/me');
        window.WebSocketManager.connect();
        const sock = wsInstances[wsInstances.length - 1];
        assert.ok(sock, 'WebSocketManager обязан был создать соединение');
        sock.onerror({}); // браузер не отдаёт статус — только событие
        await sleep(10);
        meCalls = fetchCount('/api/v1/auth/me') - before;
        assert.ok(meCalls >= 1,
            'отказ рукопожатия обязан проверяться запросом к /auth/me (иначе 401 не отличить от обрыва сети)');
        await new Promise(function (r) { setTimeout(r, 20); });

        assert.strictEqual(window.SessionExpiry.isExpired(), true,
            'WS-отказ при неавторизованной сессии обязан перевести панель в гостя');
        assert.strictEqual(global.document.body.classList.contains('auth-role-guest'), true,
            'класс роли обязан стать гостевым');
        assert.strictEqual(explanationCount(), 1, 'и здесь объяснение ровно одно');
        assert.strictEqual(window.WebSocketManager.isSuspended(), true,
            'переподключение бессмысленно — канал обязан быть приостановлен');
    });

    // -----------------------------------------------------------------------
    await check('(г) настоящий путь тоста: api-error → App.showToast молчит после истечения', async function () {
        resetEnv();
        adminNode('adminBtnToastPath');
        routes = [meAdmin(), { match: '/api/v1/cluster', status: 401, body: {} }];
        loadCore();
        // app-core.js: канонический App.showToast (единственная точка всех тостов).
        loadModule('webui/js/app-core.js');
        // Пересобираем контейнер: app-core берёт его по id при КАЖДОМ вызове.
        elements['toastContainer'] = toastContainer;
        await window.__authReady();
        try { await window.Api.cluster(); } catch (e) { /* ожидаем */ }

        // Первый 401 мог прийти до подписки — считаем базовую линию сейчас.
        const baseline = toastContainer.children.length;
        // Модули ловят ошибку и зовут тост — тексты ровно те, что даёт транспорт
        // (api.js request() кладёт «HTTP <status>: <statusText>» в err.message).
        window.App.showToast('HTTP 401: Unauthorized', 'error');
        window.App.showToast('HTTP 403: Forbidden', 'warning');
        window.App.showToast('HTTP 401: Unauthorized', 'info');
        assert.strictEqual(toastContainer.children.length, baseline,
            'после истечения сессии тосты об авторизации не должны появляться (появилось ' +
            (toastContainer.children.length - baseline) + ')');
        // А вот НЕавторизационная ошибка обязана пройти: шум не прячет проблемы.
        window.App.showToast('Нет свободного места на диске', 'error');
        assert.strictEqual(toastContainer.children.length, baseline + 1,
            'реальная ошибка обязана показаться, иначе мы «прячем проблемы»');
    });

    // -----------------------------------------------------------------------
    await check('(г) data-refresh: индикатор показывает «требуется вход», а не пачку ошибок', async function () {
        resetEnv();
        adminNode('adminBtnRefresh');
        routes = [meAdmin(), { match: '/', status: 401, body: {} }];
        loadCore();
        loadModule('webui/js/modules/data-refresh.js');
        const textEl = makeEl('dataFreshnessText');
        const iconEl = makeEl('dataFreshnessIcon');
        const nodeEl = makeEl('dataFreshness');
        elements['dataFreshnessText'] = textEl;
        elements['dataFreshnessIcon'] = iconEl;
        elements['dataFreshness'] = nodeEl;
        const DR = window.DataRefresh;

        await window.__authReady();
        try { await window.Api.cluster(); } catch (e) { /* ожидаем */ }

        // Модуль страницы ловит 401 и отмечает ошибку — как app-settings.js.
        for (let i = 0; i < 3; i++) {
            DR.markError({ status: 401, message: 'HTTP 401: Unauthorized' });
        }
        assert.strictEqual(textEl.textContent, 'требуется вход',
            'индикатор обязан показывать спокойное «требуется вход», а не «не удалось обновить»: ' +
            textEl.textContent);
        assert.strictEqual(nodeEl.classList.contains('is-error'), false,
            'индикатор не должен краснеть ошибкой в гостевом режиме');
        assert.strictEqual(iconEl.className, 'fas fa-lock', 'иконка — замок (нужен вход)');
        assert.strictEqual(DR.shouldPoll(), false, 'опрос в гостевом режиме остановлен (не 401 каждые 5 с)');

        // После входа индикатор снова живой.
        DR.setAuthExpired(false);
        DR.markFresh();
        assert.ok(/Обновлено/.test(textEl.textContent),
            'после восстановления сессии индикатор снова показывает свежесть: ' + textEl.textContent);
        assert.strictEqual(DR.shouldPoll(), true, 'опрос возобновляется после входа');
    });

    // -----------------------------------------------------------------------
    await check('статика: session-expiry.js подключён в index.html ПОСЛЕ auth.js', function () {
        const html = read('webui/index.html');
        const authAt = html.indexOf('js/modules/auth.js');
        const seAt = html.indexOf('js/modules/session-expiry.js');
        assert.ok(seAt > 0, 'index.html не подключает js/modules/session-expiry.js — window.SessionExpiry будет undefined');
        assert.ok(authAt > 0, 'index.html не подключает auth.js — проверка потеряла точку отсчёта');
        assert.ok(seAt > authAt, 'session-expiry.js обязан грузиться ПОСЛЕ auth.js');
        assert.ok(/session-expiry\.js\?v=R679/.test(html), 'у подключения обязан быть ?v=R679 (иначе кэш отдаст старый файл)');
    });

    // -----------------------------------------------------------------------
    await check('сброс состояния: reset() снимает «истекла» и возвращает опрос', function () {
        resetEnv();
        routes = [meAdmin(), { match: '/', status: 401, body: {} }];
        loadCore();
        return window.__authReady().then(function () {
            return window.Api.cluster().catch(function () { return null; });
        }).then(function () {
            assert.strictEqual(window.SessionExpiry.isExpired(), true);
            window.SessionExpiry.reset();
            assert.strictEqual(window.SessionExpiry.isExpired(), false,
                'reset обязан снять флаг «сессия истекла»');
            assert.strictEqual(window.SessionExpiry.stats().suppressed, 0, 'счётчики окна обязаны сброситься');
        });
    });

    // -----------------------------------------------------------------------
    await check('статика: app.js публикует resumeAfterAuth и pauseForSessionExpiry ВНУТРИ IIFE', function () {
        const appJs = read('webui/js/app.js');
        // Присваивание вне IIFE — ReferenceError, который обрывает весь файл и
        // панель не инициализируется вовсе (это уже случалось, R66b).
        assert.ok(/function resumeAfterAuth\s*\(/.test(appJs),
            'app.js не объявляет resumeAfterAuth — входу нечем вернуть админский режим');
        assert.ok(/function pauseForSessionExpiry\s*\(/.test(appJs),
            'app.js не объявляет pauseForSessionExpiry — истечение сессии не остановит циклы');
        assert.ok(/window\.App\.resumeAfterAuth\s*=\s*resumeAfterAuth/.test(appJs),
            'resumeAfterAuth не опубликован в window.App');
        assert.ok(/window\.App\.pauseForSessionExpiry\s*=\s*pauseForSessionExpiry/.test(appJs),
            'pauseForSessionExpiry не опубликован в window.App');
        // Опубликованы оба имени — и через window.App, и в возвращаемом объекте.
        assert.ok(/window\.App\.resumeAfterAuth[\s\S]{0,400}return\s*\{/.test(appJs) ||
            /return\s*\{[\s\S]{0,80}resumeAfterAuth/.test(appJs),
            'resumeAfterAuth не попадает в публичный API ui');
    });

    // -----------------------------------------------------------------------
    await check('статика: единая точка 401 подключена в api.js и websocket.js', function () {
        const apiJs = read('webui/js/modules/api.js');
        assert.ok(/noteSessionExpired\('http'/.test(apiJs) || /noteSessionExpired\(\s*'http'/.test(apiJs),
            'api.js request() не передаёт 401 в SessionExpiry — панель не узнает об истечении');
        assert.ok(/response\.status === 401/.test(apiJs), 'api.js не проверяет status===401');
        const wsJs = read('webui/js/modules/websocket.js');
        assert.ok(/SessionExpiry\.probe/.test(wsJs),
            'websocket.js не проверяет сессию при отказе рукопожатия');
        assert.ok(/sessionExpired\(\)/.test(wsJs),
            'websocket.js не учитывает истёкшую сессию перед переподключением');
        // app-core.js — единственная точка тостов.
        const coreJs = read('webui/js/app-core.js');
        assert.ok(/SessionExpiry/.test(coreJs) && /shouldSilence/.test(coreJs),
            'app-core.js showToast не гасит шум после истечения сессии');
    });

    // =========================================================================
    console.info = realInfo;
    console.error = realError;

    if (failures.length) {
        console.error('\n' + failures.length + ' проверок провалено, ' + passed + ' пройдено');
        failures.forEach(function (f) { console.error('  FAIL: ' + f); });
        process.exit(1);
    }
    console.log('\nвсе ' + passed + ' проверок пройдены');
})().catch(function (err) {
    console.info = realInfo;
    console.error = realError;
    console.error('FAIL:', err && err.stack ? err.stack : err);
    process.exit(1);
});
