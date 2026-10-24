// agents-tab-r134.test.js — R134 (2026-10-16).
//
// Запуск: node webui/js/modules/agents-tab-r134.test.js
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «нет вкладки агенты». Вкладка есть в разметке
// (index.html: data-page="agents", #agents-page), но её скрывал JS:
// backend-type-filter.js при типе движка llama_cpp ставил пункту display:none.
// На стенде, где единственный живой текстовый бэкенд — llama.cpp, это скрывало
// вкладку при ЛЮБОЙ ширине окна, а вернуть её оператор мог только чисткой
// localStorage (тот же урок, что у toggleGgufTab, R94).
//
// Проверяется:
//   1. при типе llama_cpp пункт меню НЕ скрывается;
//   2. с текущей страницы agents оператора не выкидывает;
//   3. при ollama пункт тоже виден (поведение не изменилось);
//   4. пустое состояние страницы агентов объясняет, когда они появляются
//      (ru/en, упоминание llama.cpp), а не просто «нет».
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

global.window = global;
const store = {};
global.localStorage = {
    getItem: function (k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem: function (k, v) { store[k] = String(v); },
    removeItem: function (k) { delete store[k]; }
};

const agentsNav = {
    dataset: { page: 'agents' },
    style: { display: 'none' },   // как будто предыдущий вызов спрятал пункт
    title: 'что-то',
    classList: { add() {}, remove() {}, contains() { return false; } }
};
let switchedTo = null;
global.window.ui = {
    currentPage: 'agents',
    switchPage: function (p) { switchedTo = p; }
};
global.document = {
    querySelector: function (sel) {
        return sel === '.nav-item[data-page="agents"]' ? agentsNav : null;
    },
    querySelectorAll: function () { return []; },
    getElementById: function () { return null; },
    addEventListener: function () {}
};

require('./backend-type-filter.js');
const BTF = global.window.BackendTypeFilter;

let checks = 0;
function check(name, fn) { fn(); checks++; console.log('  ✓ ' + name); }

check('R134: при llama_cpp вкладка «Агенты» НЕ скрывается', function () {
    if (!BTF || typeof BTF.toggleAgentsTab !== 'function') {
        throw new Error('toggleAgentsTab не опубликован модулем');
    }
    BTF.toggleAgentsTab('llama_cpp');
    assert.notStrictEqual(agentsNav.style.display, 'none',
        'вкладка снова скрыта при llama_cpp — оператор её не увидит (исходная жалоба)');
    assert.strictEqual(agentsNav.style.display, '', 'пункт меню должен показываться штатным стилем');
    assert.strictEqual(agentsNav.title, '', 'подсказка «скрыто» осталась висеть');
});

check('R134: с открытой страницы агентов оператора не выкидывает', function () {
    switchedTo = null;
    global.window.ui.currentPage = 'agents';
    BTF.toggleAgentsTab('llama_cpp');
    assert.strictEqual(switchedTo, null,
        'оператора увели на dashboard только потому, что движок llama.cpp (страница агентов существует)');
});

check('R134: при ollama вкладка тоже видна (поведение не изменилось)', function () {
    agentsNav.style.display = 'none';
    BTF.toggleAgentsTab('ollama');
    assert.strictEqual(agentsNav.style.display, '', 'при ollama вкладка обязана быть видна');
});

check('R134: в исходнике фильтра нет скрытия вкладки агентов', function () {
    const src = fs.readFileSync(path.join(__dirname, 'backend-type-filter.js'), 'utf8');
    const i = src.lastIndexOf('toggleAgentsTab');
    const body = src.slice(i, i + 500);
    assert.ok(body.indexOf("display = 'none'") === -1,
        'в toggleAgentsTab снова появилось display = none');
});

check('R134: пустое состояние объясняет, когда агенты появляются', function () {
    const ru = fs.readFileSync(path.join(__dirname, '..', 'i18n', 'ru.js'), 'utf8');
    const en = fs.readFileSync(path.join(__dirname, '..', 'i18n', 'en.js'), 'utf8');
    const ruKey = /"agents\.no_agents":\s*"([^"]+)"/.exec(ru);
    const enKey = /"agents\.no_agents":\s*"([^"]+)"/.exec(en);
    assert.ok(ruKey && ruKey[1].indexOf('llama.cpp') !== -1,
        'пустое состояние (ru) не объясняет, что при кластере из llama.cpp агентов не будет: ' +
        (ruKey ? ruKey[1] : 'ключ не найден'));
    assert.ok(enKey && enKey[1].indexOf('llama.cpp') !== -1,
        'пустое состояние (en) не объясняет причину: ' + (enKey ? enKey[1] : 'ключ не найден'));
});

console.log('\nagents-tab-r134: ' + checks + ' проверок пройдено');
