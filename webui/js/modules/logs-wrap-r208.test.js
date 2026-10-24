// logs-wrap-r208.test.js — R208 (2026-10-21).
// Живая жалоба: поле с ошибкой в системных логах нельзя прочитать — выводилось обрезанным.
// Причина: flex-элемент без min-width:0 и без переноса длинных токенов.
// Тест фиксирует контракт: перенос + копируемость + полный текст в title.
'use strict';
const assert = require('assert');
const fs = require('fs');
const path = require('path');
let checks = 0;
function check(name, fn) { fn(); checks++; console.log('  ✓ ' + name); }

const css = fs.readFileSync(path.join(__dirname, '..', '..', 'css', 'pages.css'), 'utf8');
const js = fs.readFileSync(path.join(__dirname, 'renderers.js'), 'utf8');

check('log-message переносит текст и длинные токены', function () {
  const m = css.match(/\.log-message\s*\{[\s\S]*?\}/);
  assert.ok(m, 'правило .log-message не найдено');
  const rule = m[0];
  assert.ok(/min-width:\s*0/.test(rule), 'нет min-width:0 — flex-элемент не сожмётся: ' + rule);
  assert.ok(/white-space:\s*pre-wrap/.test(rule), 'нет white-space:pre-wrap — строки не переносятся');
  assert.ok(/overflow-wrap:\s*anywhere/.test(rule), 'нет overflow-wrap — длинный токен (URL/JSON) обрежется');
  assert.ok(/user-select:\s*text/.test(rule), 'текст должен выделяться и копироваться');
});

check('карточка лога выравнивает элементы по верху (многострочное сообщение)', function () {
  const m = css.match(/\.log-entry\s*\{[\s\S]*?\}/);
  assert.ok(m, 'правило .log-entry не найдено');
  assert.ok(/align-items:\s*flex-start/.test(m[0]), 'нет align-items:flex-start — многострочный текст центрируется');
});

check('полный текст сообщения доступен в title', function () {
  assert.ok(/<div class="log-entry" title="\$\{escapeHtml\(log\.time/.test(js),
    'в шаблоне лога нет title с полным текстом — длинную ошибку не прочитать наведением');
});

console.log('\nOK: ' + checks + ' проверок пройдено');
