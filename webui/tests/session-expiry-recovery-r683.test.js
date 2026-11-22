// webui/tests/session-expiry-recovery-r683.test.js — R683 (2026-11-22)
// ОДИН 401 НЕ ИМЕЕТ ПРАВА ЗАМОРОЗИТЬ ПАНЕЛЬ НАВСЕГДА.
//
// ЖИВОЙ ДЕФЕКТ (замер на стенде, образ r261120-r682 через CDP). В живую панель
// администратора инжектируется РОВНО ОДИН ответ 401 на /api/v1/cluster, все
// остальные запросы идут как обычно, /auth/me продолжает отвечать
// authenticated:true. Результат замера:
//
//   t+2.5s  auth=false expired=true ws=false  «Отключено»  overlay=flex
//   t+30.0s auth=false expired=true ws=false  «Отключено»  overlay=flex
//   последующий УСПЕШНЫЙ запрос панель не оживляет
//
// То есть R679 («первый же 401 — это истёкшая сессия») не подтверждает вывод и
// не имеет обратного хода: quietNoisyChannels() гасит авто-обновление, WS, SSE,
// live-tail и монитор, websocket.js отказывается подключаться, пока isExpired(),
// а снять флаг может только reset() из явного входа. Панель остаётся мёртвой до
// ручного входа или F5 — для оператора это ровно «WebUI повис, не прогружая
// страницу».
//
// ЧТО ПРОВЕРЯЕТСЯ ЗДЕСЬ (контракт, а не «выглядит красиво»):
//   (а) после перехода в гостя модуль сам спрашивает /auth/me; ответ
//       authenticated:true снимает переход: state админа, класс роли admin,
//       админские действия снова видимы, слой/полоса закрыты, каналы подняты
//       (App.resumeAfterAuth), перезагрузки НЕТ;
//   (б) настоящая истёкшая сессия (authenticated:false) остаётся гостем: ровно
//       одно объяснение, каналы НЕ поднимаются, лишних запросов нет;
//   (в) проверка не удалась (балансер недоступен) — панель остаётся гостем, но
//       попыток ровно две (нет бесконечного цикла);
//   (г) гостевая загрузка (401 до входа) проверку не запускает вовсе —
//       подтверждать нечего, объяснение уже показано;
//   (д) явный вход после ложной тревоги не ломается (reset отменяет проверку);
//   (е) статика: app.js публикует resumeAfterAuth ВНУТРИ IIFE и verifyNow есть.
//
// Запуск: node webui/tests/session-expiry-recovery-r683.test.js
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
// Минимальный DOM (тот же уровень честности, что у session-expiry-r679.test.js:
// достаточно, чтобы auth.js собрал слой/полосу/чип, а тест видел, СКОЛЬКО
// объяснений показано и виден ли админский узел)
// ===========================================================================

const elements = {};
const adminOnly = [];

function makeClassList() {
    const set = new Set();
    return {
        add: (c) => set.add(c),
        remove: (c) => set.delete(c),
        contains: (c) => set.has(c),
        toggle: (c, on) => { if (on === undefined) { set.has(c) ? set.delete(c) : set.add(c); } else if (on) { set.add(c); } else { set.delete(c); } }
    };
}

function makeEl(id) {
    const attrs = {};
    const children = [];
    const el = {
        id: id || '', tagName: 'DIV', innerHTML: '', textContent: '', value: '', className: '',
        style: {}, dataset: {}, disabled: false, children, parentNode: null,
        classList: makeClassList(), _listeners: {},
        addEventListener(ev, fn) { (this._listeners[ev] = this._listeners[ev] || []).push(fn); },
        removeEventListener() {},
        dispatch(ev, payload) { (this._listeners[ev] || []).forEach((fn) => fn(payload || {})); },
        appendChild(child) { child.parentNode = this; children.push(child); if (child.id) { elements[child.id] = child; } return child; },
        removeChild(child) { const i = children.indexOf(child); if (i >= 0) { children.splice(i, 1); } child.parentNode = null; if (child.id && elements[child.id] === child) { delete elements[child.id]; } return child; },
        remove() { if (this.parentNode) { this.parentNode.removeChild(this); } },
        setAttribute(n, v) { attrs[n] = String(v); },
        getAttribute(n) { return Object.prototype.hasOwnProperty.call(attrs, n) ? attrs[n] : null; },
        hasAttribute(n) { return Object.prototype.hasOwnProperty.call(attrs, n); },
        removeAttribute(n) { delete attrs[n]; },
        querySelector() { return null; },
        querySelectorAll() { return []; },
        focus() {}, contains(other) { return other === el; }
    };
    return el;
}

global.document = {
    readyState: 'complete',
    body: makeEl('__body'),
    documentElement: makeEl('__html'),
    createElement: (tag) => makeEl(),
    createTextNode: (t) => ({ textContent: t }),
    getElementById: (id) => elements[id] || null,
    querySelector: (sel) => (sel === '.header-actions' ? makeEl('headerActions') : null),
    querySelectorAll: (sel) => (sel === '[data-admin-only]' ? adminOnly.slice() : []),
    addEventListener: function (ev, fn) { (this._listeners = this._listeners || {}); (this._listeners[ev] = this._listeners[ev] || []).push(fn); },
    _listeners: {}
};
elements['__body'] = global.document.body;
global.document.body.classList = makeClassList();

// sessionStorage
const sessionStore = {};
global.window = global;
global.sessionStorage = {
    getItem: (k) => (Object.prototype.hasOwnProperty.call(sessionStore, k) ? sessionStore[k] : null),
    setItem: (k, v) => { sessionStore[k] = String(v); },
    removeItem: (k) => { delete sessionStore[k]; }
};
global.location = { reloads: 0, reload: function () { this.reloads++; }, href: 'http://127.0.0.1:18083/', origin: 'http://127.0.0.1:18083', protocol: 'http:' };
global.MutationObserver = function () { this.observe = function () {}; };
global.requestAnimationFrame = (fn) => setTimeout(fn, 0);
global.Headers = function () { this._h = {}; this.has = () => false; this.set = () => {}; };
global.Response = undefined;

// шина событий window
const windowListeners = {};
global.window.addEventListener = function (ev, fn) { (windowListeners[ev] = windowListeners[ev] || []).push(fn); };
global.window.removeEventListener = function () {};
global.window.dispatchEvent = function (ev) {
    (windowListeners[ev.type] || []).forEach(function (fn) { try { fn(ev); } catch (e) { /* подписчик не ломает тест */ } });
    return true;
};
global.CustomEvent = function (type, init) { this.type = type; this.detail = (init && init.detail) || null; };

// консоль: console.info глушим в массив (модуль пишет туда сводку), console.warn
// тоже, а console.error НЕ трогаем — иначе раннер теста теряет свои же ✗-строки.
const consoleInfo = [];
console.info = function () { consoleInfo.push(Array.from(arguments).join(' ')); };
console.warn = function () {};

// ===========================================================================
// Управляемый fetch: 401 приходит ЖИВЫМ путём (Api.request / websocket handshake),
// а не вызовом note()
// ===========================================================================
let routes = [];
let fetchLog = [];

function makeResponse(rule) {
    const bodyText = typeof rule.body === 'string' ? rule.body : JSON.stringify(rule.body || {});
    return {
        ok: rule.status >= 200 && rule.status < 300,
        status: rule.status,
        statusText: rule.status === 401 ? 'Unauthorized' : 'OK',
        headers: { get: () => 'application/json' },
        json: () => Promise.resolve(typeof rule.body === 'string' ? JSON.parse(rule.body) : (rule.body || {})),
        text: () => Promise.resolve(bodyText)
    };
}

global.fetch = function (url, opts) {
    const u = String(url);
    fetchLog.push({ url: u, opts: opts || {} });
    for (let i = 0; i < routes.length; i++) {
        if (u.indexOf(routes[i].match) >= 0) {
            const rule = routes[i];
            if (rule.networkError) { return Promise.reject(new Error('network down')); }
            return Promise.resolve(makeResponse(rule));
        }
    }
    return Promise.resolve(makeResponse({ status: 200, body: {} }));
};

// ===========================================================================
// Загрузка модулей WebUI (classic-script'ы)
// ===========================================================================
function loadModule(rel) { new Function(read(rel))(); }

function resetEnv() {
    Object.keys(elements).forEach((k) => { delete elements[k]; });
    global.document.body.children.length = 0;
    elements['__body'] = global.document.body;
    adminOnly.length = 0;
    fetchLog = [];
    routes = [];
    consoleInfo.length = 0;
    Object.keys(windowListeners).forEach((k) => { delete windowListeners[k]; });
    Object.keys(sessionStore).forEach((k) => { delete sessionStore[k]; });
    global.document.body.classList = makeClassList();
    global.location.reloads = 0;
    delete global.Auth;
    delete global.SessionExpiry;
    delete global.Api;
    delete global.WebSocketManager;
    delete global.DataRefresh;
    delete global.__olAppInitialized;
    delete global.App;
    delete global.WEBUI_CONFIG;
}

function loadCore() {
    loadModule('webui/js/modules/auth.js');
    loadModule('webui/js/modules/session-expiry.js');
    loadModule('webui/js/modules/api.js');
}

function meAdmin() {
    return { match: '/api/v1/auth/me', status: 200, body: { authenticated: true, usersConfigured: true, user: { name: 'skyadmin', role: 'admin' }, csrf: 'csrf-683' } };
}
function meGuest() {
    return { match: '/api/v1/auth/me', status: 200, body: { authenticated: false, usersConfigured: true, user: null, csrf: '' } };
}

function adminNode(id) {
    const el = makeEl(id || ('adminBtn' + adminOnly.length));
    el.setAttribute('data-admin-only', '1');
    el.style.display = '';
    adminOnly.push(el);
    return el;
}

function overlayVisible() {
    const ov = elements['authOverlay'];
    return !!(ov && ov.style.display === 'flex');
}
function explanationCount() {
    let n = 0;
    const bar = elements['authBanner'];
    if (bar && bar.style.display !== 'none') { n++; }
    if (overlayVisible()) { n++; }
    return n;
}
function fetchCount(match) { return fetchLog.filter((f) => f.url.indexOf(match) >= 0).length; }

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/** resumeRecorder — подставной App.resumeAfterAuth: мы обязаны ВИДЕТЬ его вызов. */
function installResumeRecorder() {
    const rec = { calls: 0 };
    global.App = {
        resumeAfterAuth: function () { rec.calls++; return true; },
        pauseForSessionExpiry: function () { return true; }
    };
    return rec;
}

// ===========================================================================
// Сценарии
// ===========================================================================
(async function main() {
    console.log('session-expiry-recovery-r683.test.js — R683\n');

    // -----------------------------------------------------------------------
    await check('(а) ложная тревога: одиночный 401 снимается сам — админский режим, каналы, без reload', async function () {
        resetEnv();
        global.WEBUI_CONFIG = { SESSION_EXPIRY_VERIFY_DELAY_MS: 30 };
        const adminBtn = adminNode('addBackendBtnR683');
        const resume = installResumeRecorder();
        routes = [meAdmin(), { match: '/api/v1/cluster', status: 401, body: { error: 'Unauthorized: spurious' } }];
        loadCore();
        await window.__authReady();
        assert.strictEqual(window.Auth.isAdmin(), true, 'сценарий обязан начинаться вошедшим админом');

        // Живой путь: панель запрашивает состояние, приходит ОДИН 401.
        let failed = null;
        try { await window.Api.cluster(); } catch (e) { failed = e; }
        assert.ok(failed && failed.status === 401, '401 обязан дойти до панели');
        // Немедленная реакция R679 сохранена: гость + одно объяснение.
        assert.strictEqual(window.Auth.state().authenticated, false, 'R679: первый 401 сразу переводит в гостя');
        assert.strictEqual(window.SessionExpiry.isExpired(), true, 'R679: состояние «истекла» выставлено');
        assert.strictEqual(explanationCount(), 1, 'объяснение ровно одно');
        const overlayAtLatch = overlayVisible();

        // R683: панель обязана ПРОВЕРИТЬ вывод и снять ложную тревогу сама.
        await sleep(400);

        assert.strictEqual(window.SessionExpiry.isExpired(), false,
            'R683: сессия жива (/auth/me → authenticated:true) — «истекла» обязано быть снято, иначе панель мертва навсегда');
        const st = window.Auth.state();
        assert.strictEqual(st.authenticated, true, 'R683: state снова админский');
        assert.strictEqual(st.user && st.user.name, 'skyadmin', 'R683: пользователь восстановлен из /auth/me');
        assert.strictEqual(st.csrf, 'csrf-683',
            'R683: CSRF восстановлен — иначе после «оживления» панели не ушла бы ни одна мутация');
        assert.strictEqual(global.document.body.classList.contains('auth-role-admin'), true,
            'R683: класс роли снова admin');
        assert.strictEqual(global.document.body.classList.contains('auth-role-guest'), false,
            'R683: класс гостя снят (панель не в двух ролях)');
        assert.notStrictEqual(adminBtn.style.display, 'none', 'R683: админские действия снова видимы');
        assert.strictEqual(adminBtn.hasAttribute('disabled'), false, 'R683: админские действия снова активны');
        assert.strictEqual(overlayVisible(), false, 'R683: слой входа убран после ложной тревоги');
        assert.strictEqual(explanationCount(), 0, 'R683: объяснений не осталось');
        assert.strictEqual(resume.calls, 1,
            'R683: каналы обязаны подниматься (App.resumeAfterAuth) — WS/SSE/опрос/монитор, иначе «Подключение…» навсегда');
        assert.strictEqual(global.location.reloads, 0, 'R683: перезагрузки нет — режим возвращается на месте');
        assert.ok(fetchCount('/api/v1/auth/me') >= 2, 'R683: проверка идёт через /auth/me (тот же оракул, что у boot-гейта)');
        // Диагностика: факт ложной тревоги виден в stats и одной строкой в консоли.
        const stats = window.SessionExpiry.stats();
        assert.strictEqual(stats.recovered, 1, 'R683: stats.recovered считает снятые ложные тревоги');
        assert.ok(consoleInfo.some((l) => /ложная тревога снята/i.test(l)),
            'R683: снятие ложной тревоги объясняется одной строкой в консоли, найдено: ' + JSON.stringify(consoleInfo.slice(0, 3)));
        assert.ok(overlayAtLatch === true || overlayAtLatch === false, 'слой на момент перехода — зафиксирован');
    });

    // -----------------------------------------------------------------------
    await check('(б) настоящая истёкшая сессия остаётся гостем: одно объяснение, каналы не поднимаются', async function () {
        resetEnv();
        global.WEBUI_CONFIG = { SESSION_EXPIRY_VERIFY_DELAY_MS: 30 };
        const adminBtn = adminNode('adminBtnRealExpiry');
        const resume = installResumeRecorder();
        routes = [meAdmin()];
        loadCore();
        await window.__authReady();               // старт: админ
        routes = [meGuest(), { match: '/api/v1/cluster', status: 401, body: {} }];
        assert.strictEqual(window.Auth.isAdmin(), true, 'старт — админ');

        try { await window.Api.cluster(); } catch (e) { /* 401 */ }
        assert.strictEqual(window.SessionExpiry.isExpired(), true, 'первый 401 переводит в гостя');
        await sleep(400);

        assert.strictEqual(window.SessionExpiry.isExpired(), true,
            'R683: сервер сказал authenticated:false — состояние «истекла» обязано остаться');
        assert.strictEqual(window.Auth.state().authenticated, false, 'остаёмся гостем');
        assert.strictEqual(global.document.body.classList.contains('auth-role-guest'), true, 'класс роли — гость');
        assert.strictEqual(explanationCount(), 1, 'объяснение ровно одно (не два и не ноль)');
        assert.strictEqual(resume.calls, 0, 'R683: каналы НЕ поднимаются, сессии действительно нет');
        assert.strictEqual(global.location.reloads, 0, 'перезагрузки нет');
        assert.strictEqual(adminBtn.style.display, 'none', 'админские действия остаются скрытыми');
        assert.strictEqual(window.SessionExpiry.stats().recovered, 0, 'ложной тревоги не было — счётчик пуст');
    });

    // -----------------------------------------------------------------------
    await check('(в) проверка не удалась (балансер недоступен): остаёмся гостем, попыток ровно две', async function () {
        resetEnv();
        global.WEBUI_CONFIG = { SESSION_EXPIRY_VERIFY_DELAY_MS: 20 };
        const resume = installResumeRecorder();
        routes = [meAdmin(), { match: '/api/v1/cluster', status: 401, body: {} }];
        loadCore();
        await window.__authReady();
        // Дальше сервер недоступен: обрыв, а не отказ в правах.
        routes = [{ match: '/api/v1/auth/me', networkError: true }, { match: '/api/v1/cluster', status: 401, body: {} }];
        try { await window.Api.cluster(); } catch (e) { /* 401 */ }
        assert.strictEqual(window.SessionExpiry.isExpired(), true, 'гость выставлен');
        await sleep(500);

        assert.strictEqual(window.SessionExpiry.isExpired(), true,
            'R683: недоступный балансер — это НЕ доказательство живой сессии; остаёмся гостем');
        assert.strictEqual(resume.calls, 0, 'каналы не поднимаем');
        const stats = window.SessionExpiry.stats();
        assert.strictEqual(stats.verifyAttempts, 2,
            'R683: ровно две попытки проверки (нет бесконечного цикла), было ' + stats.verifyAttempts);
        assert.ok(explanationCount() === 1, 'объяснение остаётся одно');
    });

    // -----------------------------------------------------------------------
    await check('(г) гостевая загрузка: 401 до входа НЕ запускает проверку и не плодит объяснений', async function () {
        resetEnv();
        global.WEBUI_CONFIG = { SESSION_EXPIRY_VERIFY_DELAY_MS: 20 };
        const resume = installResumeRecorder();
        routes = [meGuest(), { match: '/api/v1/cluster', status: 401, body: {} }];
        loadCore();
        await window.__authReady();                       // страница открыта гостем
        assert.strictEqual(window.Auth.state().authenticated, false, 'страница — гость');
        const meBefore = fetchCount('/api/v1/auth/me');
        try { await window.Api.cluster(); } catch (e) { /* 401 */ }
        await sleep(300);
        assert.strictEqual(window.SessionExpiry.isExpired(), false,
            'R683: у гостя нечего подтверждать — перехода в «истекла» нет');
        assert.strictEqual(fetchCount('/api/v1/auth/me'), meBefore,
            'R683: лишних запросов к /auth/me не появилось (' + meBefore + ' → ' + fetchCount('/api/v1/auth/me') + ')');
        assert.strictEqual(resume.calls, 0, 'каналы не поднимаем');
    });

    // -----------------------------------------------------------------------
    await check('(д) явный вход после ложной тревоги не ломается: reset отменяет отложенную проверку', async function () {
        resetEnv();
        global.WEBUI_CONFIG = { SESSION_EXPIRY_VERIFY_DELAY_MS: 40 };
        const resume = installResumeRecorder();
        routes = [meAdmin(), { match: '/api/v1/cluster', status: 401, body: {} }];
        loadCore();
        await window.__authReady();
        try { await window.Api.cluster(); } catch (e) { /* 401 */ }
        assert.strictEqual(window.SessionExpiry.isExpired(), true, 'гость выставлен');

        // Явный вход ДО срабатывания проверки: сама проверка обязана замолчать.
        const stats0 = window.SessionExpiry.reset();
        assert.strictEqual(stats0.expired, false, 'reset снимает «истекла»');
        const attemptsAfterReset = window.SessionExpiry.stats().verifyAttempts;
        await sleep(250);
        assert.strictEqual(window.SessionExpiry.stats().verifyAttempts, attemptsAfterReset,
            'R683: отменённая проверка не выполняется после reset (иначе она перебила бы явный вход)');
        assert.strictEqual(resume.calls, 0, 'R683: reset сам каналы не трогает — это делает restoreSession');
    });

    // -----------------------------------------------------------------------
    await check('(е) статика: app.js публикует resumeAfterAuth ВНУТРИ IIFE, verifyNow экспортирован', async function () {
        const appSrc = read('webui/js/app.js');
        assert.ok(/window\.App\.resumeAfterAuth\s*=\s*resumeAfterAuth/.test(appSrc),
            'app.js обязан публиковать App.resumeAfterAuth');
        assert.ok(/window\.App\.pauseForSessionExpiry\s*=\s*pauseForSessionExpiry/.test(appSrc),
            'app.js обязан публиковать App.pauseForSessionExpiry');
        const seSrc = read('webui/js/modules/session-expiry.js');
        assert.ok(/verifyNow:\s*verifyNow/.test(seSrc), 'session-expiry.js обязан экспортировать verifyNow');
        assert.ok(/function recover\(/.test(seSrc), 'session-expiry.js обязан иметь recover()');
        assert.ok(/window\.SessionExpiry\s*=\s*api/.test(seSrc),
            'экземпляр модуля публикуется как api — по нему таймер понимает, что он всё ещё «свой»');
        const authSrc = read('webui/js/modules/auth.js');
        assert.ok(/function canExplainWithBanner\(/.test(authSrc),
            'auth.js обязан решать «полоса или слой» по состоянию приложения (canExplainWithBanner)');
    });

    // -----------------------------------------------------------------------
    await check('(ж) гостевая загрузка с ol_auth_guest_ack: слой ПОКАЗАН — единственный путь внутрь', async function () {
        // Живой дефект: флажок R667 «работаю без входа» переживает перезагрузку
        // (sessionStorage), а гостевой бут НЕ инициализирует панель. Полоса вместо
        // слоя оставляла оператора на скелете без меню входа — F5 не помогал.
        // R683: полоса вместо слоя — только для РАБОТАЮЩЕЙ панели.
        resetEnv();
        global.WEBUI_CONFIG = { SESSION_EXPIRY_VERIFY_DELAY_MS: 20 };
        adminNode('adminBtnGuestAck');
        sessionStore['ol_auth_guest_ack'] = '1';       // оператор уже отказывался входить
        routes = [meGuest()];
        loadCore();
        await window.__authReady();                    // загрузка гостем, ui.init() не звался
        assert.strictEqual(global.__olAppInitialized, undefined, 'приложение не инициализировано (гостевой бут)');
        assert.strictEqual(overlayVisible(), true,
            'R683: гостевой загрузке полосы недостаточно — без слоя у оператора нет ни одного пути внутрь');
        assert.strictEqual(explanationCount(), 1, 'объяснение ровно одно');

        // А для РАБОТАЮЩЕЙ панели правило R667 сохраняется: полоса, не слой.
        resetEnv();
        global.WEBUI_CONFIG = { SESSION_EXPIRY_VERIFY_DELAY_MS: 20 };
        global.__olAppInitialized = true;
        adminNode('adminBtnLiveAck');
        sessionStore['ol_auth_guest_ack'] = '1';
        routes = [meGuest()];
        loadCore();
        await window.__authReady();
        assert.strictEqual(overlayVisible(), false,
            'R683: в работающей панели флажок R667 по-прежнему оставляет полосу, а не слой');
        assert.ok(elements['authBanner'], 'полоса обязана быть');
        assert.strictEqual(explanationCount(), 1, 'объяснение ровно одно');
    });

    // -----------------------------------------------------------------------
    console.log('\nПройдено: ' + passed + '/' + (passed + failures.length));
    if (failures.length) {
        failures.forEach((f) => console.error('FAIL ' + f));
        process.exit(1);
    }
    console.log('ИТОГ: R683 — переход в гостя подтверждается и снимается сам\n');
})();
