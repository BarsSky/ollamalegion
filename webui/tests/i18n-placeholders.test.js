#!/usr/bin/env node
/*
 * i18n-placeholders.test.js — R83-fix (2026-10-09).
 *
 * ЗАЧЕМ. I18N.t подставлял параметры через String.replace со строковым шаблоном,
 * то есть менял ТОЛЬКО ПЕРВОЕ вхождение {name}. Любая строка, где плейсхолдер
 * встречается дважды, показывала оператору сырой "{name}" во втором месте.
 * Живая проверка баннера «потенциал параллельности не раскрыт» это и поймала:
 *   «…воркер держит 2 слотов, а балансер пропускает 1… выставьте
 *     maxConcurrentRequests={slots}…»      <- {slots} остался в тексте
 *
 * ЧТО ПРОВЕРЯЕТ:
 *   1. Подставляются ВСЕ вхождения плейсхолдера.
 *   2. Несколько разных параметров в одной строке подставляются все.
 *   3. В живом переводе monitor.alert.parallelismUnused не остаётся сырых
 *      фигурных скобок после подстановки.
 *
 * ЗАПУСК: node webui/tests/i18n-placeholders.test.js
 */

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const REPO_ROOT = path.resolve(__dirname, '..', '..');
const I18N_INDEX = path.join(REPO_ROOT, 'webui', 'js', 'i18n', 'index.js');
const RU = path.join(REPO_ROOT, 'webui', 'js', 'i18n', 'ru.js');
const EN = path.join(REPO_ROOT, 'webui', 'js', 'i18n', 'en.js');

let failures = 0;
function check(name, ok, detail) {
  console.log((ok ? '  PASS  ' : '  FAIL  ') + name + (!ok && detail ? ' — ' + detail : ''));
  if (!ok) failures++;
}

// Минимальное окружение: i18n/index.js — IIFE, которая вешает window.I18N и
// читает localStorage/navigator.
const store = {};
const sandbox = {
  console: { log() {}, warn() {}, error() {} },
  localStorage: {
    getItem(k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem(k, v) { store[k] = String(v); },
    removeItem(k) { delete store[k]; },
  },
  navigator: { language: 'ru', languages: ['ru'] },
  Intl: { DateTimeFormat: function () { return { resolvedOptions: function () { return { timeZone: 'Europe/Moscow' }; } }; } },
  setTimeout, clearTimeout,
  document: { addEventListener() {}, documentElement: { setAttribute() {}, getAttribute() { return 'ru'; } } },
};
sandbox.window = sandbox;
sandbox.globalThis = sandbox;
// index.js вешает слушатель динамической регистрации пакетов.
sandbox.addEventListener = function () {};
sandbox.removeEventListener = function () {};
sandbox.dispatchEvent = function () { return true; };
vm.createContext(sandbox);
// Пакеты переводов грузятся ДО index.js (как <script> в index.html): index.js
// в autoRegister() читает window.I18N_RU / window.I18N_EN.
vm.runInContext(fs.readFileSync(RU, 'utf8'), sandbox, { filename: RU });
vm.runInContext(fs.readFileSync(EN, 'utf8'), sandbox, { filename: EN });
vm.runInContext(fs.readFileSync(I18N_INDEX, 'utf8'), sandbox, { filename: I18N_INDEX });

const I18N = sandbox.I18N || sandbox.window.I18N;
check('I18N инициализирован', !!I18N && typeof I18N.t === 'function');

// 1. Все вхождения одного параметра.
I18N.register('ru', { 'test.repeat': 'A {x} B {x} C' });
const repeated = I18N.t('test.repeat', { x: '7' });
check('подставляются все вхождения одного параметра',
  repeated === 'A 7 B 7 C', JSON.stringify(repeated));

// 2. Несколько параметров.
I18N.register('ru', { 'test.multi': '{a}-{b}-{a}' });
const multi = I18N.t('test.multi', { a: 'x', b: 'y' });
check('подставляются все параметры и все их вхождения',
  multi === 'x-y-x', JSON.stringify(multi));

// 3. Живой ключ баннера: после подстановки не должно остаться фигурных скобок.
const live = I18N.t('monitor.alert.parallelismUnused', { backend: 'cppworker-gpu', slots: 2, effective: 1 });
check('баннер: сырых плейсхолдеров не осталось',
  live.indexOf('{') === -1 && live.indexOf('}') === -1, JSON.stringify(live));
check('баннер: оба числа и совет по настройке на месте',
  live.indexOf('2') !== -1 && live.indexOf('1') !== -1 && live.indexOf('maxConcurrentRequests=2') !== -1,
  JSON.stringify(live));

console.log('');
if (failures > 0) {
  console.log('ИТОГ: провалено проверок — ' + failures);
  process.exit(1);
}
console.log('ИТОГ: все проверки пройдены');
