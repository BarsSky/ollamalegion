// monitor-i18n-keys.test.js — R238d (2026-10-25): локализация монитора.
//
// ЖАЛОБА. «У монитора идёт своя локализация, переключение языка надо объединить
// с общей в WebUI; и в мониторе не все поля локализованы».
//
// ПРИЧИНА (найдена в DOM): monitor.html вообще не применял data-i18n при
// загрузке — работала только небольшая карта текстов updateMonitorTexts, поэтому
// заголовки панели раскладки, кнопки, подсказка и др. оставались на языке
// разметки. Плюс часть ключей отсутствовала в словарях.
//
// ЭТОТ ТЕСТ фиксирует:
//   1. каждый ключ, который использует монитор (data-i18n/-html/-title/
//      -placeholder в monitor.html и T('...') в js/monitor + js/modules/monitor-*),
//      есть И в ru, И в en;
//   2. тексты с HTML-разметкой (help modal) размечены data-i18n-html, а не
//      data-i18n — иначе I18N.apply вставит их текстом.
//
// Запуск: node webui/js/modules/monitor-i18n-keys.test.js

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

// Минимальное окружение для словарей (они пишут window.I18N_RU/EN).
global.window = global;
global.document = { documentElement: { setAttribute() {} }, addEventListener() {}, querySelectorAll() { return []; } };
global.localStorage = { getItem() { return null; }, setItem() {} };
global.CustomEvent = function () {};

require('../../js/i18n/ru.js');
require('../../js/i18n/en.js');
const RU = global.I18N_RU;
const EN = global.I18N_EN;
assert.ok(RU && EN, 'словари ru/en должны загрузиться');

let checks = 0;
function check(name, fn) { fn(); checks++; console.log('  ok ' + name); }

const ROOT = path.join(__dirname, '..', '..');
const HTML = path.join(ROOT, 'monitor.html');

function keysFromHtml(text) {
  const out = [];
  const re = /data-i18n(?:-html|-title|-placeholder)?="([^"]+)"/g;
  let m;
  while ((m = re.exec(text))) { out.push(m[1]); }
  return out;
}

function keysFromJs(text) {
  const out = [];
  // T(...) — монитор, tr(...) — демо-UI и auth-плашки (R238d).
  const re = /\b(?:MA\.)?(?:T|tr)\(\s*['"]([^'"]+)['"]/g;
  let m;
  while ((m = re.exec(text))) { out.push(m[1]); }
  return out;
}

function jsFiles() {
  const dirs = [
    path.join(ROOT, 'js', 'monitor'),
    path.join(ROOT, 'js', 'modules')
  ];
  const files = [];
  for (const dir of dirs) {
    for (const f of fs.readdirSync(dir)) {
      if (!f.endsWith('.js') || f.includes('.test.')) continue;
      if (dir.endsWith('modules') && !f.startsWith('monitor-')) continue;
      files.push(path.join(dir, f));
    }
  }
  return files;
}

const used = new Map();
function add(key, where) {
  if (!key || key.endsWith('.')) return; // динамический префикс вроде 'plan.strategy.' + s
  if (!used.has(key)) used.set(key, []);
  const list = used.get(key);
  if (list.indexOf(where) < 0 && list.length < 3) list.push(where);
}

for (const k of keysFromHtml(fs.readFileSync(HTML, 'utf8'))) add(k, 'monitor.html');
for (const f of jsFiles()) {
  for (const k of keysFromJs(fs.readFileSync(f, 'utf8'))) add(k, path.basename(f));
}
// R238d: демо-баннер/панель/сценарий и auth-плашка тоже должны быть переведены.
for (const rel of ['js/modules/demo-mode.js', 'js/modules/auth.js']) {
  const f = path.join(ROOT, rel);
  for (const k of keysFromJs(fs.readFileSync(f, 'utf8'))) add(k, path.basename(f));
}

check('ключи монитора есть и в ru, и в en', function () {
  const missingRu = [], missingEn = [];
  for (const [k, where] of used) {
    if (!Object.prototype.hasOwnProperty.call(RU, k)) missingRu.push(k + ' (' + where.join(', ') + ')');
    if (!Object.prototype.hasOwnProperty.call(EN, k)) missingEn.push(k + ' (' + where.join(', ') + ')');
  }
  assert.deepStrictEqual(missingRu, [], 'нет в ru: ' + missingRu.join('; '));
  assert.deepStrictEqual(missingEn, [], 'нет в en: ' + missingEn.join('; '));
});

check('help modal размечен data-i18n-html (там <strong>/<br>/<code>)', function () {
  const html = fs.readFileSync(HTML, 'utf8');
  const helpBlock = html.slice(html.indexOf('id="helpModal"'), html.indexOf('id="topoTooltip"'));
  const htmlKeys = (helpBlock.match(/data-i18n-html="([^"]+)"/g) || []).length;
  const plainKeys = (helpBlock.match(/data-i18n="([^"]+)"/g) || []).length;
  assert.ok(htmlKeys >= 8, 'в helpModal должно быть >= 8 data-i18n-html, найдено ' + htmlKeys);
  assert.strictEqual(plainKeys, 0, 'в helpModal не должно остаться data-i18n (HTML превратится в текст)');
});

check('язык хранится в общем ключе ollamalegion_lang', function () {
  const index = fs.readFileSync(path.join(ROOT, 'js', 'i18n', 'index.js'), 'utf8');
  assert.ok(/STORAGE_KEY\s*=\s*'ollamalegion_lang'/.test(index), 'i18n/index.js должен хранить язык в ollamalegion_lang');
  assert.ok(/apply:\s*apply/.test(index), 'I18N.apply должен быть экспортирован');
  const monitor = fs.readFileSync(HTML, 'utf8');
  assert.ok(monitor.indexOf("localStorage.setItem('ollamalegion_lang'") >= 0, 'селектор монитора должен писать тот же ключ');
});

console.log('\nOK: ' + checks + ' checks, keys: ' + used.size);
