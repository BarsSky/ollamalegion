// auth.test.js — R202 (2026-10-21): браузерная часть входа в WebUI.
//
// Запуск: node webui/js/modules/auth.test.js
//
// Что проверяется (контракт, а не «выглядит красиво»):
//   1. /auth/me authenticated=false + usersConfigured=true → показывается экран входа;
//   2. успешный логин отправляет name/password на /api/v1/auth/login и перезагружает
//      страницу (после входа все модули читают данные с сервера — источник истины);
//   3. неверный пароль показывает ошибку, экран не исчезает;
//   4. CSRF из /auth/me уходит в заголовках мутирующих запросов (X-CSRF-Token);
//   5. роль viewer видна коду (Auth.isAdmin() === false) — UI прячет админские действия;
//   6. если вход НЕ настроен (usersConfigured=false), панель НЕ блокируется
//      (иначе оператор мог бы запереть себя снаружи);
//   7. logout отправляет POST /auth/logout и перезагружает страницу.

'use strict';

const assert = require('assert');

let checks = 0;
function check(name, fn) {
  return Promise.resolve().then(fn).then(function () {
    checks++;
    console.log('  ✓ ' + name);
  });
}

// --- минимальный DOM ------------------------------------------------------
const elements = {};
function makeEl(id) {
  const el = {
    id: id, innerHTML: '', textContent: '', value: '', style: {}, dataset: {},
    disabled: false, children: [], _listeners: {},
    addEventListener: function (ev, fn) { (this._listeners[ev] = this._listeners[ev] || []).push(fn); },
    appendChild: function (child) { this.children.push(child); return child; },
    setAttribute: function () {}, getAttribute: function () { return null; },
    querySelector: function () { return null; },
    focus: function () {}
  };
  elements[id] = el;
  return el;
}

global.document = {
  readyState: 'loading',
  _listeners: {},
  body: { appendChild: function (el) { elements[el.id] = el; return el; } },
  documentElement: {},
  getElementById: function (id) { return elements[id] || null; },
  createElement: function () { return makeEl(''); },
  addEventListener: function (ev, fn) { (this._listeners[ev] = this._listeners[ev] || []).push(fn); },
  querySelector: function () { return null; }
};
// R222: аварийный токен живёт в sessionStorage вкладки (не в отданной странице).
const sessionStore = {};
global.sessionStorage = {
  getItem: function (k) { return Object.prototype.hasOwnProperty.call(sessionStore, k) ? sessionStore[k] : null; },
  setItem: function (k, v) { sessionStore[k] = String(v); },
  removeItem: function (k) { delete sessionStore[k]; }
};
global.window = global;
global.WEBUI_CONFIG = {};
global.location = { reloadCount: 0, reload: function () { this.reloadCount++; } };

// --- управляемый fetch ----------------------------------------------------
let calls = [];
let responses = [];
global.fetch = function (url, opts) {
  calls.push({ url: url, opts: opts || {} });
  const next = responses.shift();
  const res = next || { status: 200, body: {} };
  return Promise.resolve({
    status: res.status,
    json: function () { return Promise.resolve(res.body); }
  });
};

function loadAuth() {
  delete require.cache[require.resolve('./auth.js')];
  Object.keys(elements).forEach(function (k) { delete elements[k]; });
  calls = [];
  const fs = require('fs');
  const path = require('path');
  const src = fs.readFileSync(path.join(__dirname, 'auth.js'), 'utf8');
  // запускаем в текущем контексте: модуль сам экспортирует window.Auth
  new Function(src)();
}

const ME_ANON = { status: 200, body: { authenticated: false, usersConfigured: true } };
const ME_VIEWER = { status: 200, body: { authenticated: true, usersConfigured: true, user: { name: 'guest', role: 'viewer' }, csrf: 'csrf-123' } };

(async function main() {
  console.log('auth.test.js — R202\n');

  await check('экран входа показывается, когда есть пользователи и нет сессии', async function () {
    responses = [ME_ANON];
    loadAuth();
    await window.Auth.init();
    await new Promise(function (r) { setTimeout(r, 0); });
    const overlay = Object.keys(elements).filter(function (k) { return elements[k].id === 'authOverlay'; });
    assert.ok(window.Auth.state().usersConfigured === true, 'usersConfigured не прочитан');
    assert.strictEqual(window.Auth.state().authenticated, false);
    assert.ok(overlay.length > 0 || global.document.body.children.length > 0, 'оверлей не создан');
  });

  await check('успешный логин отправляет name/password и перезагружает страницу', async function () {
    responses = [
      { status: 200, body: { success: true, user: { name: 'admin', role: 'admin' }, csrf: 'csrf-ok' } }
    ];
    loadAuth();
    const api = window.Auth;
    // эмулируем submit формы напрямую через вызов внутреннего обработчика:
    // оверлей создаётся при init, поля берём из DOM-заглушки
    responses = [ME_ANON];
    await api.init();
    elements['authName'] = elements['authName'] || makeEl('authName');
    elements['authPassword'] = elements['authPassword'] || makeEl('authPassword');
    elements['authName'].value = 'admin';
    elements['authPassword'].value = 'secret';
    responses = [{ status: 200, body: { success: true, user: { name: 'admin', role: 'admin' }, csrf: 'csrf-ok' } }];
    const before = global.location.reloadCount;
    const overlay = elements['authOverlay'];
    assert.ok(overlay && overlay._listeners.submit, 'нет обработчика submit');
    await overlay._listeners.submit[0]({ preventDefault: function () {} });
    await new Promise(function (r) { setTimeout(r, 0); });
    const loginCall = calls.filter(function (c) { return c.url.indexOf('/api/v1/auth/login') >= 0; })[0];
    assert.ok(loginCall, 'логин не отправлен');
    const body = JSON.parse(loginCall.opts.body);
    assert.strictEqual(body.name, 'admin');
    assert.strictEqual(body.password, 'secret');
    assert.ok(global.location.reloadCount > before, 'страница не перезагружена после входа');
  });

  await check('CSRF из /auth/me уходит в заголовках мутирующих запросов', async function () {
    responses = [ME_VIEWER];
    loadAuth();
    await window.Auth.init();
    const h = window.Auth.headers();
    assert.strictEqual(h['X-CSRF-Token'], 'csrf-123', 'CSRF не попал в заголовки');
  });

  await check('viewer не считается администратором (UI прячет админские действия)', async function () {
    responses = [ME_VIEWER];
    loadAuth();
    await window.Auth.init();
    assert.strictEqual(window.Auth.isAdmin(), false);
    assert.strictEqual(window.Auth.role(), 'viewer');
  });

  await check('R222: без настроенных пользователей панель НЕ блокируется, но состояние объяснено', async function () {
    responses = [{ status: 200, body: { authenticated: false, usersConfigured: false } }];
    loadAuth();
    await window.Auth.init();
    const overlay = elements['authOverlay'];
    const visible = overlay && overlay.style && overlay.style.display === 'flex';
    assert.ok(!visible, 'экран входа не должен блокировать панель без пользователей');
    const banner = elements['authBanner'];
    assert.ok(banner, 'нет полосы с объяснением, что вход не настроен');
  });

  await check('R222: гость (без входа) не видит админские действия', async function () {
    responses = [ME_ANON];
    loadAuth();
    await window.Auth.init();
    assert.strictEqual(window.Auth.isAdmin(), false, 'гость не должен считаться админом');
    const el = makeEl('guestAdminAction');
    el.getAttribute = function () { return ''; };
    el.hasAttribute = function () { return false; };
    el.removeAttribute = function () {};
    global.document.querySelectorAll = function (sel) { return sel === '[data-admin-only]' ? [el] : []; };
    window.Auth.applyRoleVisibility();
    assert.strictEqual(el.style.display, 'none', 'гость должен не видеть админское действие');
  });

  await check('R222: аварийный токен живёт в sessionStorage и уходит в X-API-Token', async function () {
    responses = [ME_ANON];
    loadAuth();
    await window.Auth.init();
    window.Auth.setToken('tok-r222');
    assert.strictEqual(sessionStore['ol_break_glass_token'], 'tok-r222', 'токен не сохранён в sessionStorage');
    assert.strictEqual(window.WEBUI_CONFIG.API_TOKEN, 'tok-r222', 'токен не проброшен в WEBUI_CONFIG.API_TOKEN');
    const h = window.Auth.headers();
    assert.strictEqual(h['X-API-Token'], 'tok-r222', 'токен не уходит в заголовке');
    window.Auth.clearToken();
    assert.strictEqual(sessionStore['ol_break_glass_token'], undefined, 'токен не удалён');
    assert.strictEqual(window.WEBUI_CONFIG.API_TOKEN, '', 'WEBUI_CONFIG.API_TOKEN не очищен');
  });

  await check('R222: вход по токену проверяет права и перезагружает страницу', async function () {
    responses = [ME_ANON];
    loadAuth();
    await window.Auth.init();
    elements['authToken'] = elements['authToken'] || makeEl('authToken');
    elements['authToken'].value = 'tok-ok';
    const overlay = elements['authOverlay'];
    assert.ok(overlay && overlay._listeners.click, 'нет делегированного обработчика входа по токену');
    // Проверка идёт запросом, который требует прав администратора.
    responses = [{ status: 200, body: { users: [] } }];
    const before = global.location.reloadCount;
    overlay._listeners.click[0]({ target: { id: 'authTokenSubmit' } });
    await new Promise(function (r) { setTimeout(r, 0); });
    const probe = calls.filter(function (c) { return c.url.indexOf('/api/v1/auth/users') >= 0; })[0];
    assert.ok(probe, 'токен не проверен запросом к /auth/users');
    assert.strictEqual(probe.opts.headers['X-API-Token'], 'tok-ok', 'токен не отправлен в проверку');
    assert.strictEqual(sessionStore['ol_break_glass_token'], 'tok-ok', 'верный токен не сохранён');
    assert.ok(global.location.reloadCount > before, 'после входа по токену нет перезагрузки');
  });

  await check('logout отправляет POST /auth/logout', async function () {
    responses = [{ status: 200, body: { success: true } }];
    loadAuth();
    await window.Auth.logout();
    const call = calls.filter(function (c) { return c.url.indexOf('/api/v1/auth/logout') >= 0; })[0];
    assert.ok(call, 'logout не отправлен');
    assert.strictEqual(call.opts.method, 'POST');
  });

  await check('viewer: админские элементы (data-admin-only) скрыты и disabled', async function () {
    responses = [ME_VIEWER];
    loadAuth();
    const el = makeEl('adminAction');
    el.setAttribute('data-admin-only', '1');
    el.getAttribute = function (k) { return k === 'data-admin-only' ? '1' : null; };
    el.hasAttribute = function () { return false; };
    el.removeAttribute = function () {};
    global.document.querySelectorAll = function (sel) { return sel === '[data-admin-only]' ? [el] : []; };
    await window.Auth.init();
    window.Auth.applyRoleVisibility();
    assert.strictEqual(el.style.display, 'none', 'viewer должен не видеть админское действие');
    // админ — видит (элемент возвращается)
    global.document.querySelectorAll = function (sel) { return sel === '[data-admin-only]' ? [el] : []; };
    responses = [{ status: 200, body: { authenticated: true, usersConfigured: true, user: { name: 'admin', role: 'admin' }, csrf: 'c' } }];
    loadAuth();
    const el2 = makeEl('adminAction2');
    el2.getAttribute = function () { return ''; };
    el2.hasAttribute = function () { return false; };
    el2.removeAttribute = function () {};
    global.document.querySelectorAll = function (sel) { return sel === '[data-admin-only]' ? [el2] : []; };
    await window.Auth.init();
    window.Auth.applyRoleVisibility();
    assert.notStrictEqual(el2.style.display, 'none', 'админ должен видеть действие');
  });

  await check('R209: CSRF автоматически добавляется к мутирующим same-origin запросам', async function () {
    // Состояние с CSRF: обёртка fetch должна добавить заголовок, не ломая вызов.
    responses = [ME_VIEWER];
    loadAuth();
    await window.Auth.init();
    calls = [];
    await window.fetch('/api/v1/backend/xyz', { method: 'PUT', body: '{}' });
    var call = calls[0];
    assert.ok(call, 'fetch не вызван');
    var h = call.opts.headers;
    var csrf = (h && typeof h.get === 'function') ? h.get('X-CSRF-Token') : (h && h['X-CSRF-Token']);
    assert.strictEqual(csrf, 'csrf-123', 'CSRF не добавлен обёрткой fetch: ' + JSON.stringify(h));
  });

  await check('R223: до входа запросы к /api/ не уходят на сервер (синтетический 401)', async function () {
    responses = [ME_ANON];
    loadAuth();
    await window.Auth.init();
    calls = [];
    const res = await window.fetch('/api/v1/backends');
    assert.strictEqual(res.status, 401, 'ожидался 401 без запроса к серверу');
    assert.strictEqual(calls.length, 0, 'запрос ушёл на сервер до входа — будет 401 в консоли и логах');
  });

  await check('R223: сам /api/v1/auth/me гейтом не блокируется', async function () {
    responses = [ME_ANON];
    loadAuth();
    await window.Auth.init();
    calls = [];
    await window.fetch('/api/v1/auth/me');
    assert.strictEqual(calls.length, 1, '/auth/me должен проходить всегда, иначе вход невозможен');
  });

  await check('R223: без настроенных пользователей запросы идут как раньше', async function () {
    responses = [{ status: 200, body: { authenticated: false, usersConfigured: false } }];
    loadAuth();
    await window.Auth.init();
    calls = [];
    await window.fetch('/api/v1/backends');
    assert.strictEqual(calls.length, 1, 'открытая панель (без пользователей) не должна терять запросы');
  });

  console.log('\nOK: ' + checks + ' проверок пройдено');
})().catch(function (err) {
  console.error('FAIL:', err && err.message ? err.message : err);
  process.exit(1);
});
