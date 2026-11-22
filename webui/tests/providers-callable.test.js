// providers-callable.test.js — R493 (2026-11-17): «провайдер зарегистрирован» ≠
// «провайдер работает».
//
// ЖИВОЙ ДЕФЕКТ, ради которого этот набор. При переводе обновления страниц на
// единый реестр (data-refresh.js) регистрация провайдера диагностики была
// вынесена ЗА пределы IIFE модуля — там `load` уже не в области видимости, и
// провайдер падал с `ReferenceError: load is not defined` при первом вызове.
// Со стороны оператора это выглядело как «кнопка «Обновить» не обновляет
// диагностику»; статические проверки молчали, потому что искали строку
// `DataRefresh.register('diagnostics'` — она в файле была.
//
// Поэтому здесь провайдеры не ищутся по тексту, а РЕАЛЬНО ВЫЗЫВАЮТСЯ: модуль
// загружается в node с минимальными стабами и его зарегистрированные функции
// исполняются. Ошибка ссылки, отсутствующая зависимость или вызов undefined —
// всё это красный тест, а не «зелёный по строке».
//
// Запуск: node webui/tests/providers-callable.test.js
'use strict';

const assert = require('assert');
const path = require('path');

let passed = 0;
const failures = [];
const checks = [];
function check(name, fn) { checks.push([name, fn]); }

// --- окружение -----------------------------------------------------------------

const providers = {};
const requests = [];

function makeElement(id) {
    const attrs = {};
    return {
        id: id,
        dataset: {},
        style: {},
        textContent: '',
        innerHTML: '',
        className: '',
        value: '',
        children: [],
        classList: {
            add() {}, remove() {}, toggle() {}, contains() { return false; },
        },
        getAttribute(n) { return Object.prototype.hasOwnProperty.call(attrs, n) ? attrs[n] : null; },
        setAttribute(n, v) { attrs[n] = v; },
        appendChild(c) { this.children.push(c); return c; },
        querySelector() { return null; },
        querySelectorAll() { return []; },
        addEventListener() {},
        closest() { return null; },
        focus() {},
    };
}

const elements = {};
global.window = global;
global.document = {
    getElementById(id) {
        if (!elements[id]) elements[id] = makeElement(id);
        return elements[id];
    },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    addEventListener() {},
    visibilityState: 'visible',
    createElement(tag) { return makeElement('new-' + tag); },
    // R680: список моделей AutoTune пересобирается через DocumentFragment
    // (коллектор→фрагмент→restore), как и карточки моделей в app-data.js:831.
    // Без этой строки провайдер падал на document.createDocumentFragment, и
    // «провайдер вызывается» краснело не по делу.
    createDocumentFragment() {
        const frag = makeElement('#fragment');
        frag.isFragment = true;
        return frag;
    },
    body: makeElement('body'),
};

global.window.DataRefresh = {
    register(page, fn) { (providers[page] = providers[page] || []).push({ fn, from: page }); return true; },
    registerGlobal(fn) { (providers['*'] = providers['*'] || []).push({ fn, from: '*' }); return true; },
    markFresh() {}, markError() {}, isAuto() { return true; }, shouldPoll() { return true; },
};

// Api-заглушка: каждый метод — Promise с пустым объектом; вызовы записываются,
// чтобы тест видел, что провайдер действительно сходил за данными.
global.window.Api = new Proxy({}, {
    get(_t, name) {
        if (name === 'then') return undefined; // не притворяемся thenable
        return function () {
            requests.push(String(name));
            return Promise.resolve({});
        };
    },
});
global.fetch = function (url) {
    requests.push('fetch:' + String(url));
    return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({}), text: () => Promise.resolve('') });
};
global.window.I18N = { t: (k, vars) => (vars ? String(k) : k) };
global.window.Utils = { escapeHtml: (s) => String(s), setText() {} };

// --- модули, чьи провайдеры проверяем ------------------------------------------

const MODULES = [
    { file: 'diagnostics.js', page: 'diagnostics' },
    { file: 'autotune_settings.js', page: 'settings' },
];

for (const m of MODULES) {
    require(path.join(__dirname, '..', 'js', 'modules', m.file));
}

check('модули страниц зарегистрировали провайдеров', function () {
    const missing = MODULES.filter((m) => !providers[m.page] || providers[m.page].length === 0);
    assert.deepStrictEqual(missing.map((m) => m.file), [],
        'нет провайдера у страниц: ' + missing.map((m) => m.page).join(', '));
});

check('у каждой страницы РОВНО один провайдер (повторный mount не плодит дубликаты)', async function () {
    // mount() страницы диагностики вызывается refreshPage на каждый тик — раньше
    // это добавляло ещё одного провайдера и ещё один запрос на тик.
    if (global.window.Diagnostics && typeof global.window.Diagnostics.mount === 'function') {
        global.window.Diagnostics.mount();
        global.window.Diagnostics.mount();
    }
    const dup = Object.keys(providers).filter((p) => providers[p].length > 1);
    assert.deepStrictEqual(dup, [], 'провайдеры задвоены у страниц: ' + dup.join(', '));
});

check('каждый зарегистрированный провайдер ВЫЗЫВАЕТСЯ без ошибок', async function () {
    const pages = Object.keys(providers);
    assert.ok(pages.length > 0, 'провайдеров нет вовсе — тест бессмыслен');
    for (const page of pages) {
        for (const entry of providers[page]) {
            // Именно вызов: `ReferenceError: load is not defined` (живой дефект)
            // проявляется только здесь, а не в поиске строки по файлу.
            await entry.fn();
        }
    }
    assert.ok(requests.length > 0, 'провайдеры отработали, но ни одного запроса не сделали — данные не обновляются');
});

(async function main() {
    for (const [name, fn] of checks) {
        try {
            await fn();
            passed++;
            console.log('  \u2713 ' + name);
        } catch (e) {
            failures.push(name + ': ' + (e && e.message));
            console.error('  \u2717 ' + name + ' — ' + (e && e.message));
        }
    }
    console.log('\n' + (failures.length ? 'FAILED: ' + failures.length : 'ИТОГ: все проверки пройдены (' + passed + ')'));
    if (failures.length) {
        failures.forEach(function (f) { console.error(' - ' + f); });
        process.exit(1);
    }
    process.exit(0);
})();
