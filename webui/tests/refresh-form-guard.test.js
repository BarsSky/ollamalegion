// webui/tests/refresh-form-guard.test.js — R512 (2026-11-18): авто-обновление
// не перерисовывает страницу, пока оператор вводит текст в форму.
//
// ЖИВАЯ ЖАЛОБА. «Постоянное обновление WebUI держит новую информацию, но не даёт
// нормально взаимодействовать с формами — постоянно их скрывая». Механизм:
// раз в 5 с тик вызывает refreshPage(currentPage); для настроек это loadSettings() +
// SettingsRegistry.load() + loadBackendLimits() — все поля заново заполняются из
// ответа сервера, поэтому набранный символ затирался, курсор уезжал, секции
// схлопывались.
//
// Проверяется: (1) модуль guard корректно отличает «ввод» от «клик по кнопке» и
// «поле вне активной страницы»; (2) тик реально вызывает guard ПЕРЕД перерисовкой;
// (3) модуль подключён в index.html (иначе window.RefreshGuard === undefined и
// проверка молча не работает — это отдельный класс дефекта «код есть, не подключён»).
//
// Запуск: node webui/tests/refresh-form-guard.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const root = path.join(__dirname, '..');
const moduleSrc = fs.readFileSync(path.join(root, 'js', 'modules', 'refresh-guard.js'), 'utf8');
const appJs = fs.readFileSync(path.join(root, 'js', 'app.js'), 'utf8');
const indexHtml = fs.readFileSync(path.join(root, 'index.html'), 'utf8');

let passed = 0;
const failures = [];
function check(name, fn) {
    try {
        fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' — ' + (e && e.message));
    }
}

// Загружаем модуль в песочницу с заглушкой window (как это делает браузер).
function loadGuard() {
    const sandboxWindow = {};
    const sandbox = { window: sandboxWindow, document: { activeElement: null } };
    vm.createContext(sandbox);
    vm.runInContext(moduleSrc, sandbox, { filename: 'refresh-guard.js' });
    assert.ok(sandboxWindow.RefreshGuard, 'модуль refresh-guard.js не выставил window.RefreshGuard');
    return sandboxWindow.RefreshGuard;
}

// Фейковые DOM-узлы: важны только tagName/type/isContentEditable и contains().
function el(tagName, props) {
    const node = Object.assign({ tagName: tagName }, props || {});
    node.contains = function (child) { return child === node; };
    return node;
}

check('поля ввода считаются вводом: input text/password/number, textarea, select', function () {
    const g = loadGuard();
    ['text', 'password', 'number', 'search', 'url'].forEach(function (type) {
        assert.ok(g.isEditable(el('INPUT', { type: type })), 'input[type=' + type + '] должен считаться вводом');
    });
    assert.ok(g.isEditable(el('TEXTAREA')), 'textarea должен считаться вводом');
    assert.ok(g.isEditable(el('SELECT')), 'select должен считаться вводом');
    assert.ok(g.isEditable(el('DIV', { isContentEditable: true })), 'contenteditable должен считаться вводом');
});

check('кнопки и чекбоксы НЕ блокируют авто-обновление', function () {
    const g = loadGuard();
    ['button', 'submit', 'reset', 'checkbox', 'radio', 'hidden'].forEach(function (type) {
        assert.ok(!g.isEditable(el('INPUT', { type: type })),
            'input[type=' + type + '] не является набором текста — блокировать обновление нельзя');
    });
    assert.ok(!g.isEditable(el('BUTTON')), 'button не является набором текста');
    assert.ok(!g.isEditable(null), 'null не является вводом');
});

check('ввод вне активной страницы не блокирует обновление', function () {
    const g = loadGuard();
    const settingsPage = el('DIV');
    const otherInput = el('INPUT', { type: 'text' });
    // contains() у фейковой страницы возвращает true только для «своих» детей.
    settingsPage.contains = function (child) { return child === insideInput; };
    const insideInput = el('INPUT', { type: 'text' });
    assert.ok(g.isEditingIn(settingsPage, insideInput), 'поле внутри активной страницы обязано блокировать тик');
    assert.ok(!g.isEditingIn(settingsPage, otherInput), 'поле вне активной страницы не должно блокировать тик');
    assert.ok(!g.isEditingIn(null, insideInput), 'без страницы блокировать нечего');
});

check('тик авто-обновления вызывает guard ПЕРЕД refreshPage', function () {
    const start = appJs.indexOf('refreshTimer = setInterval(');
    assert.ok(start !== -1, 'в app.js нет периодического цикла');
    const end = appJs.indexOf('}, interval);', start);
    assert.ok(end !== -1, 'не найден конец тела периодического цикла');
    const body = appJs.slice(start, end);
    const guardPos = body.indexOf('RefreshGuard.isEditingIn(');
    const renderPos = body.indexOf('refreshPage(currentPage)');
    assert.ok(guardPos !== -1, 'в тике нет проверки RefreshGuard.isEditingIn — формы будут затираться при вводе');
    assert.ok(renderPos !== -1, 'в тике нет перерисовки страницы (проверка выродилась)');
    assert.ok(guardPos < renderPos, 'проверка ввода обязана стоять ДО перерисовки страницы');
    assert.ok(/document\.getElementById\(currentPage \+ '-page'\)/.test(body),
        'guard должен получать контейнер АКТИВНОЙ страницы, иначе он проверяет не то поле');
});

check('модуль подключён в index.html (иначе guard молча выключен)', function () {
    assert.ok(/js\/modules\/refresh-guard\.js/.test(indexHtml),
        'index.html не подключает js/modules/refresh-guard.js — window.RefreshGuard не появится и проверка в тике не сработает');
});

console.log('');
if (failures.length) {
    console.error('Провалов: ' + failures.length);
    failures.forEach(function (f) { console.error('  - ' + f); });
    process.exit(1);
}
console.log('Пройдено проверок: ' + passed);
