// users-admin.test.js — R222 (2026-10-22): управление пользователями WebUI.
//
// Запуск: node webui/js/modules/users-admin.test.js
//
// Контракт:
//   1. список берётся с сервера и роли подписаны по-человечески (admin/гость);
//   2. добавление шлёт PUT с name/password/role и перерисовывается из ОТВЕТА;
//   3. удаление шлёт DELETE с именем;
//   4. гость (не админ) данные не грузит вовсе — никаких 401/403 в консоли;
//   5. пароль не показывается и не хранится в UI после сохранения.
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
    id: id || '', innerHTML: '', textContent: '', value: '', className: '', type: '',
    colSpan: 1, style: {}, disabled: false, children: [], _listeners: {},
    addEventListener: function (ev, fn) { (this._listeners[ev] = this._listeners[ev] || []).push(fn); },
    appendChild: function (child) { this.children.push(child); return child; },
    focus: function () {}
  };
  // innerHTML в заглушке обязан очищать детей: модуль очищает таблицу именно им,
  // иначе строки копятся между перерисовками и тест «зеленеет» на неправильном коде.
  let html = '';
  Object.defineProperty(el, 'innerHTML', {
    get: function () { return html; },
    set: function (v) { html = String(v); el.children.length = 0; }
  });
  if (id) { elements[id] = el; }
  return el;
}

global.document = {
  readyState: 'loading',
  _listeners: {},
  getElementById: function (id) { return elements[id] || null; },
  createElement: function () { return makeEl(''); },
  addEventListener: function (ev, fn) { (this._listeners[ev] = this._listeners[ev] || []).push(fn); }
};
global.window = global;
global.WEBUI_CONFIG = {};
global.window.prompt = function () { return null; };
global.window.confirm = function () { return true; };

// --- управляемый fetch ----------------------------------------------------
let calls = [];
let responses = [];
global.fetch = function (url, opts) {
  calls.push({ url: url, opts: opts || {} });
  const next = responses.shift() || { status: 200, body: {} };
  return Promise.resolve({
    status: next.status,
    json: function () { return Promise.resolve(next.body); }
  });
};

function loadModule() {
  delete require.cache[require.resolve('./users-admin.js')];
  Object.keys(elements).forEach(function (k) { delete elements[k]; });
  // элементы, к которым модуль обращается
  makeEl('usersTableBody');
  makeEl('usersHint');
  makeEl('userAddBtn');
  makeEl('userNewName');
  makeEl('userNewRole');
  makeEl('userNewPassword');
  makeEl('sessionsTableBody');
  makeEl('sessionsHint');
  calls = [];
  const fs = require('fs');
  const path = require('path');
  const src = fs.readFileSync(path.join(__dirname, 'users-admin.js'), 'utf8');
  new Function(src)();
}

function setAdmin(isAdmin) {
  global.Auth = {
    csrf: function () { return 'csrf-222'; },
    token: function () { return ''; },
    isAdmin: function () { return isAdmin; }
  };
  global.__authReady = function () { return Promise.resolve(); };
}

const USERS = [
  { name: 'test-admin', role: 'admin' },
  { name: 'guest', role: 'viewer' }
];

(async function main() {
  console.log('users-admin.test.js — R222\n');

  await check('список пользователей с понятными ролями', async function () {
    setAdmin(true);
    loadModule();
    responses = [{ status: 200, body: { users: USERS } }];
    await window.UsersAdmin.reload();
    const tbody = elements['usersTableBody'];
    assert.strictEqual(tbody.children.length, 2, 'ожидались две строки');
    const labels = tbody.children.map(function (tr) { return tr.children[1].textContent; });
    assert.ok(labels.indexOf('администратор') >= 0, 'роль admin подписана непонятно: ' + labels.join(','));
    assert.ok(labels.some(function (l) { return l.indexOf('гость') >= 0; }), 'роль viewer не названа гостем: ' + labels.join(','));
    const getCall = calls.filter(function (c) { return c.url.indexOf('/api/v1/auth/users') >= 0; })[0];
    assert.strictEqual(getCall.opts.headers['X-CSRF-Token'], 'csrf-222', 'CSRF не передан');
  });

  await check('добавление шлёт PUT и перерисовывает список из ответа', async function () {
    setAdmin(true);
    loadModule();
    elements['userNewName'].value = 'operator';
    elements['userNewRole'].value = 'viewer';
    elements['userNewPassword'].value = 'password-1';
    // init() подтягивает список своим GET — даём ему ответ и ждём тик.
    responses = [{ status: 200, body: { users: USERS } }];
    await window.UsersAdmin.init();
    await new Promise(function (r) { setTimeout(r, 0); });
    responses = [{ status: 200, body: { success: true, users: USERS.concat([{ name: 'operator', role: 'viewer' }]) } }];
    elements['userAddBtn']._listeners.click[0]();
    await new Promise(function (r) { setTimeout(r, 0); });
    const putCall = calls.filter(function (c) { return c.opts.method === 'PUT'; })[0];
    assert.ok(putCall, 'PUT не отправлен');
    const body = JSON.parse(putCall.opts.body);
    assert.strictEqual(body.name, 'operator');
    assert.strictEqual(body.role, 'viewer');
    assert.strictEqual(body.password, 'password-1');
    assert.strictEqual(elements['userNewPassword'].value, '', 'пароль остался в поле после сохранения');
    assert.strictEqual(elements['usersTableBody'].children.length, 3, 'список не перерисован из ответа');
  });

  await check('удаление шлёт DELETE с именем', async function () {
    setAdmin(true);
    loadModule();
    responses = [{ status: 200, body: { users: USERS } }];
    await window.UsersAdmin.reload();
    const row = elements['usersTableBody'].children[0];
    const delBtn = row.children[2].children[1];
    responses = [{ status: 200, body: { success: true, users: [USERS[1]] } }];
    delBtn._listeners.click[0]();
    await new Promise(function (r) { setTimeout(r, 0); });
    const delCall = calls.filter(function (c) { return c.opts.method === 'DELETE'; })[0];
    assert.ok(delCall, 'DELETE не отправлен');
    assert.ok(delCall.url.indexOf('name=test-admin') >= 0, 'имя пользователя не в запросе: ' + delCall.url);
    assert.strictEqual(elements['usersTableBody'].children.length, 1, 'список не обновлён после удаления');
  });

  await check('гость не грузит список пользователей', async function () {
    setAdmin(false);
    loadModule();
    responses = [];
    await window.UsersAdmin.init();
    await new Promise(function (r) { setTimeout(r, 0); });
    const getCall = calls.filter(function (c) { return c.url.indexOf('/api/v1/auth/users') >= 0; })[0];
    assert.ok(!getCall, 'гость сделал запрос к /auth/users (получит 403 в консоль)');
  });

  await check('ошибка сервера показывается, а не проглатывается', async function () {
    setAdmin(true);
    loadModule();
    responses = [{ status: 403, body: { error: 'Forbidden: требуются права администратора' } }];
    await window.UsersAdmin.reload();
    const hint = elements['usersHint'];
    assert.strictEqual(hint.style.display, 'block', 'ошибка не показана');
    assert.ok(String(hint.textContent).indexOf('права администратора') >= 0, 'текст ошибки не дошёл: ' + hint.textContent);
  });

  await check('R225: активные сессии — кто, откуда, текущая и отзыв', async function () {
    setAdmin(true);
    loadModule();
    responses = [{
      status: 200,
      body: {
        sessions: [
          { id: 'aaaa1111', user: 'test-admin', role: 'admin', created: '2026-10-22T09:00:00Z', last_seen: '2026-10-22T10:00:00Z', ip: '192.0.2.20', user_agent: 'Edge/130', current: true },
          { id: 'bbbb2222', user: 'guest', role: 'viewer', created: '2026-10-21T18:00:00Z', last_seen: '2026-10-22T09:30:00Z', ip: '192.0.2.77', user_agent: 'Firefox/131', current: false }
        ]
      }
    }];
    await window.UsersAdmin.reloadSessions();
    const tbody = elements['sessionsTableBody'];
    assert.strictEqual(tbody.children.length, 2, 'ожидались две сессии');
    const first = tbody.children[0];
    assert.ok(String(first.children[4].textContent).indexOf('192.0.2.20') >= 0, 'не показан адрес входа');
    assert.ok(String(first.children[3].textContent).indexOf('эта вкладка') >= 0, 'не помечена текущая сессия');
    assert.strictEqual(first.children[5].textContent, 'текущая', 'у текущей сессии не должно быть кнопки отзыва');

    const guestRow = tbody.children[1];
    const revoke = guestRow.children[5].children[0];
    assert.ok(revoke, 'нет кнопки «Завершить» у чужой сессии');
    responses = [{ status: 200, body: { success: true } }];
    revoke._listeners.click[0]();
    await new Promise(function (r) { setTimeout(r, 0); });
    const del = calls.filter(function (c) { return c.opts.method === 'DELETE'; })[0];
    assert.ok(del, 'DELETE сессии не отправлен');
    assert.ok(del.url.indexOf('id=bbbb2222') >= 0, 'неверный id в запросе: ' + del.url);
  });

  await check('R225: гость не грузит и список сессий', async function () {
    setAdmin(false);
    loadModule();
    responses = [];
    await window.UsersAdmin.init();
    await new Promise(function (r) { setTimeout(r, 0); });
    const req = calls.filter(function (c) { return c.url.indexOf('/api/v1/auth/sessions') >= 0; })[0];
    assert.ok(!req, 'гость сделал запрос к /auth/sessions');
  });

  console.log('\nOK: ' + checks + ' проверок пройдено');
})().catch(function (e) {
  console.log('FAIL: ' + e.message);
  process.exit(1);
});
