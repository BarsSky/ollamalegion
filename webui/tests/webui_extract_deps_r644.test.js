// webui_extract_deps_r644.test.js — R644 (2026-11-19): самопроверка скрипта,
// который считает зависимости вынесенного WebUI-куска.
//
// ЗАЧЕМ. Скрипт появился из-за трёх одинаковых ошибок этого цикла (R640 забыл
// `backendChoices`, R643 — `escapeHtml` из деструктуризации и `OLLAMA_PARAMS` —
// таблицу `const`). Если сам скрипт врёт, он вернёт ту же ошибку, но с видом
// «проверено инструментом» — это хуже, чем считать руками. Поэтому здесь фикстуры,
// повторяющие форму тех ошибок.
//
// Запуск: node webui/tests/webui_extract_deps_r644.test.js
'use strict';

const assert = require('assert');
const path = require('path');

let passed = 0;
const failures = [];
function check(name, fn) {
    try {
        fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' \u2014 ' + (e && e.message));
    }
}

const { depsOf, declaredNames, bodyOf } = require(path.join(__dirname, '..', '..', 'scripts', 'webui_extract_deps.js'));

// Фикстура ровно той формы, на которой я ошибался в R643: `escapeHtml` приходит из
// деструктуризации, `OLLAMA_PARAMS` — таблица `const`, `_t` — стрелочная функция.
const FIXTURE = `
const Renderers = (function () {
    const { escapeHtml, formatMB } = Utils;
    const _t = (k) => window.I18N ? window.I18N.t(k) : k;
    const OLLAMA_PARAMS = { ctx: { label: 'Контекст' } };
    const PRED_THRESHOLD = 300;

    function renderOllamaParams(b) {
        var out = '';
        for (var k in OLLAMA_PARAMS) out += escapeHtml(_t('renderers.' + k)) + formatMB(b.size);
        var local = { nope: 1 };
        return out + local.nope + PRED_THRESHOLD;
    }

    function escapeAttr(v) {
        return String(v).replace(/"/g, '&quot;');
    }
})();
`;

check('R644: находит зависимости из деструктуризации', function () {
    const deps = depsOf(FIXTURE, ['renderOllamaParams']);
    assert.ok(deps.includes('escapeHtml'),
        'не нашёл escapeHtml (деструктуризация Utils) — вернулась ошибка R643: ' + deps.join(', '));
    assert.ok(deps.includes('formatMB'), 'не нашёл formatMB: ' + deps.join(', '));
});

check('R644: находит зависимости-таблицы const и стрелочные хелперы', function () {
    const deps = depsOf(FIXTURE, ['renderOllamaParams']);
    assert.ok(deps.includes('OLLAMA_PARAMS'),
        'не нашёл OLLAMA_PARAMS (const-таблица) — вернулась ошибка R643: ' + deps.join(', '));
    assert.ok(deps.includes('_t'), 'не нашёл _t (стрелочная функция): ' + deps.join(', '));
    assert.ok(deps.includes('PRED_THRESHOLD'), 'не нашёл PRED_THRESHOLD (константа): ' + deps.join(', '));
});

check('R644: не считает зависимостями локальные объявления и параметры', function () {
    const deps = depsOf(FIXTURE, ['renderOllamaParams']);
    assert.ok(!deps.includes('local'), 'локальная переменная попала в зависимости: ' + deps.join(', '));
    assert.ok(!deps.includes('k'), 'переменная цикла попала в зависимости: ' + deps.join(', '));
    assert.ok(!deps.includes('out'), 'локальная строка попала в зависимости: ' + deps.join(', '));
});

check('R644: не тянет функции, которые не вызываются', function () {
    const deps = depsOf(FIXTURE, ['renderOllamaParams']);
    assert.ok(!deps.includes('escapeAttr'), 'в зависимости попала несвязанная функция: ' + deps.join(', '));
});

check('R644: bodyOf не сбивается на скобках внутри строк', function () {
    const src = '\nfunction f(a) {\n  var s = "} не конец";\n  var t = `тоже }`;\n  return s + t;\n}\nfunction g() { return 1; }\n';
    const body = bodyOf(src, 'f');
    assert.ok(body, 'bodyOf вернул null');
    assert.ok(body.includes('return s + t;'), 'тело обрезалось строкой со скобкой');
    assert.ok(body.trim().endsWith('}'), 'тело не закрылось на своей скобке');
});

check('R644: вызов функции, которая НЕ выносится, считается зависимостью', function () {
    // Ситуация выноса: `renderPage` остаётся в старом файле, `renderRow` уезжает.
    // `renderRow` её не зовёт, а `renderPage` зовёт — но мы считаем зависимости
    // ВЫНОСИМОГО куска, поэтому здесь наоборот: оставляем `helper` снаружи.
    const src = `
function helper(x) { return x * 2; }
function renderRow(b) { return helper(b.size) + escapeHtml(b.name); }
function escapeHtml(s) { return String(s); }
`;
    const deps = depsOf(src, ['renderRow']);
    assert.ok(deps.includes('helper'), 'вызов внешней функции не найден: ' + deps.join(', '));
    // escapeHtml — вызывается как `escapeHtml(...)`, но объявлена в том же файле и
    // НЕ выносится: значит она тоже нужна снаружи. Скрипт обязан её показать.
    assert.ok(deps.includes('escapeHtml'), 'не показал внешнюю escapeHtml: ' + deps.join(', '));
});

check('R644: ошибка на неизвестной функции, а не пустой список', function () {
    let threw = false;
    try { depsOf(FIXTURE, ['нетТакойФункции']); } catch (e) { threw = true; }
    assert.ok(threw, 'молча вернул пусто вместо ошибки — «ноль зависимостей» соврёт');
});

if (failures.length) {
    console.error('\nwebui_extract_deps_r644: ' + failures.length + ' провал(ов)');
    process.exit(1);
}
console.log('\nOK: ' + passed + ' проверок (R644)');
