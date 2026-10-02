// state-esc.test.js — MonitorApp.esc обязан экранировать HTML по-настоящему.
//
// Запуск: node webui/js/monitor/state-esc.test.js
//
// ЗАЧЕМ. Панели Monitor печатают через esc() значения, пришедшие от клиента
// (имя модели из тела запроса, путь, id бэкенда) в innerHTML. До R-Image Phase 8
// карта подстановки в state.js была сломана: для '&', '<' и '>' она возвращала
// ТЕ ЖЕ символы, то есть не экранировала ничего, кроме кавычек — имя модели вида
// <img src=x onerror=...> исполнялось в браузере оператора (XSS с доступом к
// admin API). Тест закрепляет реальное экранирование, а не «похожесть» кода.

'use strict';

const assert = require('assert');

// state.js при загрузке трогает window/localStorage/document — даём минимум.
global.window = global;
global.localStorage = { getItem() { return null; }, setItem() { }, removeItem() { } };
global.document = {
    getElementById() { return null; },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    addEventListener() { },
    removeEventListener() { },
    documentElement: { style: {} },
    body: { style: {} },
};
global.location = { search: '', origin: 'http://localhost', href: 'http://localhost/monitor.html' };
// state.js в конце навешивает обработчики на window (resize/visibilitychange) —
// в браузере это есть всегда, в node подставляем no-op.
global.addEventListener = function () { };
global.removeEventListener = function () { };

require('./state.js');
const esc = global.window.MonitorApp && global.window.MonitorApp.esc;
assert.strictEqual(typeof esc, 'function', 'MonitorApp.esc должен быть объявлен (state.js)');

let passed = 0;
function check(name, fn) {
    fn();
    passed++;
    console.log('  \u2713 ' + name);
}

console.log('monitor/state.js: MonitorApp.esc');

check('тег из имени модели экранируется (XSS-вектор закрыт)', function () {
    const out = esc('<img src=x onerror=alert(1)>');
    assert.strictEqual(out.indexOf('<'), -1, 'в выводе не должно остаться сырого "<": ' + out);
    assert.strictEqual(out.indexOf('>'), -1, 'в выводе не должно остаться сырого ">": ' + out);
    assert.ok(out.indexOf('&lt;img') === 0, 'ожидали &lt;img..., получили: ' + out);
});

check('амперсанд экранируется первым (двойного экранирования нет)', function () {
    assert.strictEqual(esc('a & b'), 'a &amp; b');
    assert.strictEqual(esc('&lt;'), '&amp;lt;');
});

check('кавычки экранируются (атрибуты в innerHTML)', function () {
    const out = esc('" onmouseover="alert(1)');
    assert.strictEqual(out.indexOf('"'), -1, 'сырая двойная кавычка осталась: ' + out);
    assert.strictEqual(esc("'"), '&#39;');
});

check('null/undefined → пустая строка, числа → строка', function () {
    assert.strictEqual(esc(null), '');
    assert.strictEqual(esc(undefined), '');
    assert.strictEqual(esc(512), '512');
});

check('обычный текст не меняется', function () {
    assert.strictEqual(esc('sd15-q4'), 'sd15-q4');
    assert.strictEqual(esc('Qwen3.8-27B-UD-Q4_K_M'), 'Qwen3.8-27B-UD-Q4_K_M');
});

console.log('');
console.log('ИТОГ: все проверки пройдены (' + passed + ')');
