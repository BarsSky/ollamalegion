#!/usr/bin/env node
/*
 * notifications-escape.test.js — R83 (2026-09-25)
 *
 * ЗАЧЕМ. webui/js/modules/notifications.js собирал разметку через innerHTML,
 * подставляя ev.message / ev.source / ev.model БЕЗ экранирования. Туда попадает
 * внешний текст: сообщения cppworker и llama.cpp (включая сырые ошибки и пути),
 * а ev.model — это имя модели ИЗ ЗАПРОСА КЛИЕНТА. То есть клиент мог прислать
 * имя вида `<img src=x onerror=...>` и исполнить скрипт в браузере оператора —
 * хранимый XSS в панели мониторинга.
 *
 * С появлением уведомлений о провале загрузки (raw_error от llama.cpp,
 * diagnostics со списком моделей) важность выросла: теперь через уведомления
 * проходит заметно больше внешнего текста.
 *
 * ЧТО ПРОВЕРЯЕТ:
 *   1. message / source / model экранируются (нет сырых < > " ').
 *   2. severity проходит whitelist (значение уходит в имя CSS-класса —
 *      подстановка произвольной строки ломает разметку).
 *   3. Подробности (data.diagnostics) тоже экранируются.
 *
 * ЗАПУСК: node webui/tests/notifications-escape.test.js   (без зависимостей)
 */

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const REPO_ROOT = path.resolve(__dirname, '..', '..');
// NOTIFICATIONS_TARGET — хук для проверки теста на произвольной ревизии файла:
//   git show HEAD:webui/js/modules/notifications.js > /tmp/old.js
//   NOTIFICATIONS_TARGET=/tmp/old.js node webui/tests/notifications-escape.test.js  # должен УПАСТЬ
const TARGET = process.env.NOTIFICATIONS_TARGET
  ? path.resolve(process.env.NOTIFICATIONS_TARGET)
  : path.join(REPO_ROOT, 'webui', 'js', 'modules', 'notifications.js');

let failures = 0;

function check(name, ok, detail) {
  if (ok) {
    console.log('  PASS  ' + name);
  } else {
    failures++;
    console.log('  FAIL  ' + name + (detail ? ' — ' + detail : ''));
  }
}

function fakeEl(tag) {
  const el = {
    tagName: tag || 'div',
    _html: '',
    textContent: '',
    className: '',
    hidden: false,
    children: [],
    dataset: {},
    style: {},
    appendChild(child) { this.children.push(child); return child; },
    removeChild() {},
    setAttribute() {},
    removeAttribute() {},
    getAttribute() { return null; },
    addEventListener() {},
    removeEventListener() {},
    querySelector() { return null; },
    querySelectorAll() { return []; },
    classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
  };
  // В браузере `innerHTML = ''` УДАЛЯЕТ дочерние узлы. Без этого повторный
  // render() оставлял в children старые элементы, и проверки читали прошлую
  // разметку (поймано на «известный severity сохранён»).
  Object.defineProperty(el, 'innerHTML', {
    get() { return el._html; },
    set(value) {
      el._html = String(value);
      if (el._html === '') el.children = [];
    },
  });
  return el;
}

function makeSandbox() {
  const containers = new Map();
  const sandbox = {
    console: { log() {}, info() {}, warn() {}, error() {}, debug() {} },
    localStorage: { getItem() { return null; }, setItem() {}, removeItem() {} },
    setTimeout, clearTimeout, setInterval, clearInterval,
    Date, Math, JSON, Object, Array, String, Number, Boolean, RegExp, Error,
    encodeURIComponent, decodeURIComponent,
  };
  sandbox.window = sandbox;
  sandbox.globalThis = sandbox;
  sandbox.document = {
    getElementById(id) {
      if (!containers.has(id)) containers.set(id, fakeEl(id));
      return containers.get(id);
    },
    createElement(tag) { return fakeEl(tag); },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    addEventListener() {},
  };
  sandbox.__containers = containers;
  return sandbox;
}

function load(sandbox) {
  const src = fs.readFileSync(TARGET, 'utf8');
  vm.createContext(sandbox);
  vm.runInContext(src, sandbox, { filename: TARGET });
}

const XSS = '<img src=x onerror="alert(1)">';
const XSS_SCRIPT = '<script>alert(2)</script>';

console.log('notifications escape (' + path.relative(REPO_ROOT, TARGET) + ')');

// --- 1. Экранирование message/source/model ----------------------------------
{
  const sb = makeSandbox();
  load(sb);

  const mgr = sb.notifications;
  check('файл экспортирует window.notifications', !!mgr && typeof mgr.render === 'function');

  mgr.buffer = [{
    id: 'x1',
    type: 'notification',
    severity: 'error',
    source: XSS_SCRIPT,
    model: XSS,
    message: 'Модель не найдена: ' + XSS,
    timestamp: '2026-09-25T09:46:14Z',
  }];

  mgr.render('notificationsList');
  const container = sb.__containers.get('notificationsList');
  const html = container.children.length ? container.children[0].innerHTML : '';

  check('контейнер получил элемент', container.children.length === 1,
    'children=' + container.children.length);
  check('сырой <img> не попал в разметку', html.indexOf('<img') === -1, html);
  check('сырой <script> не попал в разметку', html.indexOf('<script') === -1, html);
  check('onerror не попал как атрибут', html.indexOf('onerror="') === -1, html);
  check('экранированный текст присутствует', html.indexOf('&lt;img') !== -1, html);
}

// --- 2. severity проходит whitelist -----------------------------------------
{
  const sb = makeSandbox();
  load(sb);

  const mgr = sb.notifications;
  mgr.buffer = [{
    id: 'x2',
    type: 'notification',
    severity: 'error" onmouseover="alert(1)',
    message: 'ok',
    timestamp: '2026-09-25T09:46:14Z',
  }];

  mgr.render('notificationsList');
  const li = sb.__containers.get('notificationsList').children[0];

  check('неизвестный severity сведён к info',
    li.className === 'notifications-item severity-info', 'className=' + li.className);
  check('в className нет кавычек из события', li.className.indexOf('"') === -1, li.className);

  // Известные значения по-прежнему работают.
  mgr.buffer = [{ id: 'x3', type: 'notification', severity: 'critical', message: 'ok' }];
  mgr.render('notificationsList');
  const li2 = sb.__containers.get('notificationsList').children[0];
  check('известный severity сохранён',
    li2.className === 'notifications-item severity-critical', 'className=' + li2.className);
}

// --- 3. Подробности (diagnostics) экранируются ------------------------------
{
  const sb = makeSandbox();
  load(sb);

  const mgr = sb.notifications;
  mgr.buffer = [{
    id: 'x4',
    type: 'notification',
    severity: 'error',
    source: 'load',
    model: 'qwen3.8:latest',
    message: 'Конфигурация вне границ',
    timestamp: '2026-09-25T09:46:14Z',
    data: {
      event_kind: 'load_failed',
      reason: 'config_out_of_bounds',
      raw_error: XSS_SCRIPT,
      diagnostics: {
        requested_n_ctx: 131072,
        max_vram_n_ctx: 32768,
        hard_max_n_ctx: 100000,
        suggestion: 'Снизьте n_ctx ' + XSS,
        available_models: [XSS, 'Qwen3.8-27B-UD-Q4_K_M'],
      },
    },
  }];

  mgr.render('notificationsList');
  const html = sb.__containers.get('notificationsList').children[0].innerHTML;

  check('детали отрисованы', html.indexOf('131072') !== -1, html);
  check('числа из diagnostics показаны', html.indexOf('32768') !== -1, html);
  check('в деталях нет сырого <img>', html.indexOf('<img') === -1, html);
  check('в деталях нет сырого <script>', html.indexOf('<script') === -1, html);
  check('экранированное значение из available_models', html.indexOf('&lt;img') !== -1, html);
}

// --- 4. Без diagnostics подробностей нет (не ломаем обычные уведомления) ----
{
  const sb = makeSandbox();
  load(sb);

  const mgr = sb.notifications;
  mgr.buffer = [{
    id: 'x5', type: 'notification', severity: 'warning',
    source: 'transport', model: 'm', message: 'EOF from upstream',
    timestamp: '2026-09-25T09:46:14Z',
  }];

  mgr.render('notificationsList');
  const html = sb.__containers.get('notificationsList').children[0].innerHTML;
  check('обычное уведомление отрисовано', html.indexOf('EOF from upstream') !== -1, html);
}

console.log('');
if (failures > 0) {
  console.log('ИТОГ: провалено проверок — ' + failures);
  process.exit(1);
}
console.log('ИТОГ: все проверки пройдены');
