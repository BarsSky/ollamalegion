// proxy-logs-columns-r493.test.js — R493-D4 (2026-11-11).
//
// ЖИВОЙ ДЕФЕКТ: таблица прокси-логов рендерится с 10 колонками (последняя —
// «Ошибка»), а в pages.css ширина была задана только для 9. При
// table-layout:fixed десятая колонка оставалась без размера: текст ошибки
// обрезался в ноль или наезжал на соседнюю колонку, хотя полный текст открывался
// кликом по строке. Оператор видел строку без единого символа причины.
//
// ЗАПУСК: node webui/js/modules/proxy-logs-columns-r493.test.js
//
// ЧТО ФИКСИРУЕМ (контракт, а не «так получилось»):
//   1. число колонок в рендере == число правил ширины в CSS;
//   2. последняя (широкая) колонка — с шириной auto, остальные с явной;
//   3. каждая ячейка укладывается в колонку (overflow:hidden + text-overflow);
//   4. у ячейки ошибки полный текст доступен по клику (детали строки).
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

let checks = 0;
function check(name, fn) {
  fn();
  checks++;
  console.log('  \u2713 ' + name);
}

const css = fs.readFileSync(path.join(__dirname, '..', '..', 'css', 'pages.css'), 'utf8');
const js = fs.readFileSync(path.join(__dirname, 'renderers.js'), 'utf8');

function proxyLogsSource() {
  const start = js.indexOf('function proxyLogs(');
  assert.ok(start > 0, 'в renderers.js не найдена функция proxyLogs');
  // до следующей функции того же уровня («    function name(»)
  const rest = js.slice(start + 10);
  const end = rest.indexOf('\n    function ');
  return end > 0 ? js.slice(start, start + 10 + end) : js.slice(start);
}

function headerColumnCount(src) {
  const thead = src.match(/<thead><tr>([\s\S]*?)<\/tr><\/thead>/);
  assert.ok(thead, 'в рендере не найдена шапка таблицы прокси-логов');
  return (thead[1].match(/<th>/g) || []).length;
}

function bodyColumnCount(src) {
  const row = src.match(/'<tr class="proxy-log-row[\s\S]*?'<\/tr>'/);
  assert.ok(row, 'в рендере не найдена строка таблицы прокси-логов');
  return (row[0].match(/'<td[ >]/g) || []).length;
}

function cssWidthColumns() {
  const re = /\.proxy-logs-table th:nth-child\((\d+)\)\s*\{\s*width:\s*([^;]+);/g;
  const out = [];
  let m;
  while ((m = re.exec(css)) !== null) {
    out.push({ index: parseInt(m[1], 10), width: m[2].trim() });
  }
  return out.sort(function (a, b) { return a.index - b.index; });
}

const src = proxyLogsSource();

check('шапка и строка рендерят одинаковое число колонок', function () {
  const head = headerColumnCount(src);
  const body = bodyColumnCount(src);
  assert.strictEqual(head, body,
    'шапка ' + head + ' колонок, строка ' + body + ' — данные съедут относительно заголовков');
});

check('ширина задана для КАЖДОЙ колонки таблицы', function () {
  const cols = headerColumnCount(src);
  const widths = cssWidthColumns();
  assert.strictEqual(widths.length, cols,
    'колонок в рендере ' + cols + ', а правил ширины в pages.css ' + widths.length +
    ': колонка без ширины при table-layout:fixed остаётся без размера — текст ошибки не виден');
  const expected = [];
  for (let i = 1; i <= cols; i++) expected.push(i);
  assert.deepStrictEqual(widths.map(function (w) { return w.index; }), expected,
    'правила ширины должны идти подряд с 1-й по ' + cols + '-ю колонку');
});

check('остаток ширины забирает последняя колонка (ошибка), а не промежуточная', function () {
  const widths = cssWidthColumns();
  const last = widths[widths.length - 1];
  assert.strictEqual(last.width, 'auto', 'последняя колонка должна быть auto, получено ' + last.width);
  const autoOthers = widths.filter(function (w) { return w.width === 'auto'; });
  assert.strictEqual(autoOthers.length, 1,
    'ширина auto должна быть ровно у одной колонки, найдено ' + autoOthers.length);
});

check('ячейка укладывается в колонку: overflow:hidden + text-overflow', function () {
  const m = css.match(/\.proxy-logs-table td\s*\{[\s\S]*?\}/);
  assert.ok(m, 'не найдено правило .proxy-logs-table td');
  assert.ok(/overflow:\s*hidden/.test(m[0]), 'нет overflow:hidden — текст вылезает на соседнюю колонку');
  assert.ok(/text-overflow:\s*ellipsis/.test(m[0]), 'нет text-overflow:ellipsis — обрезка без индикатора');
});

check('полный текст ошибки доступен в раскрывающемся блоке строки', function () {
  assert.ok(/proxy-log-detail[\s\S]*?proxy-log-error-full/.test(src),
    'в строке нет блока деталей с полным текстом ошибки');
});

console.log('\nOK: ' + checks + ' проверок пройдено');
