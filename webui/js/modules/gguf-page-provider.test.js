// gguf-page-provider.test.js — R493 (2026-11-17): провайдер страницы GGUF.
//
// ЖИВОЙ ДЕФЕКТ, который проверяется: «на странице GGUF информация по загруженным
// моделям на бэкендах появляется только после обновления страницы (F5)».
//
// Причин было две, и обе — в этом файле:
//   1. регистрация зависела от порядка скриптов: `register()` возвращал false,
//      если `window.GgufModule` ещё не было, и больше не вызывался (кроме
//      DOMContentLoaded). Страница оставалась без провайдера — общая кнопка её
//      не обновляла вообще;
//   2. провайдера теперь дёргает и авто-обновление (раз в REFRESH_INTERVAL),
//      поэтому `refreshDetail` не должен перерисовывать панель, пока оператор
//      вводит значение в поле — иначе набранное стирается на каждом тике.
//
// Проверки асинхронные (провайдер возвращает Promise), поэтому они собираются в
// список и прогоняются последовательно: иначе провал внутри .then() вылетал бы
// «uncaught rejection» уже после итоговой строки и отчёт врал бы «всё зелено».
//
// Запуск: node webui/js/modules/gguf-page-provider.test.js
'use strict';

const assert = require('assert');

let passed = 0;
const failures = [];
const checks = [];
function check(name, fn) {
    checks.push([name, fn]);
}

// --- окружение: минимальный DOM + реестр DataRefresh -------------------------

const detailPanel = {
    _contains: null,
    contains: function (node) { return node === detailPanel._contains; },
};

global.window = global;
global.document = {
    getElementById: function (id) { return id === 'ggufDetailPanel' ? detailPanel : null; },
    addEventListener: function () { },
    activeElement: null,
};

const providers = {};
global.window.DataRefresh = {
    register: function (page, fn) {
        (providers[page] = providers[page] || []).push(fn);
        return true;
    },
};

// GgufModule появляется ПОСЛЕ загрузки провайдера — ровно та ситуация, в которой
// прежняя реализация отказывалась регистрироваться.
require('./gguf-page-provider.js');
const P = global.window.GgufPageProvider;

/** Реализация GgufModule, записывающая вызовы в calls. */
function fakeModule(calls) {
    global.window.GgufModule = {
        refreshBackends: function () { calls.push('backends'); return Promise.resolve(); },
        refreshDetail: function () { calls.push('detail'); return Promise.resolve(); },
        refreshActiveDownloads: function () { calls.push('downloads'); return Promise.resolve(); },
    };
}

function runProvider() {
    return Promise.all((providers['gguf'] || []).map(function (fn) { return fn(); }));
}

check('провайдер регистрируется даже если GgufModule ещё не загрузился', function () {
    assert.ok(providers['gguf'] && providers['gguf'].length === 1,
        'нет провайдера страницы gguf — общая кнопка не обновит страницу');
});

check('функции рендерера берутся в момент вызова, а не на регистрации', function () {
    const calls = [];
    fakeModule(calls);
    return runProvider().then(function () {
        assert.deepStrictEqual(calls.sort(), ['backends', 'detail', 'downloads'],
            'провайдер не вызвал функции рендерера: ' + calls.join(','));
    });
});

check('пока оператор вводит значение в панели деталей, детали не перерисовываются', function () {
    const calls = [];
    fakeModule(calls);
    const input = { tagName: 'INPUT' };
    detailPanel._contains = input;
    global.document.activeElement = input;
    assert.strictEqual(P._isTypingInDetail(), true, 'фокус в поле панели должен определяться как ввод');
    return runProvider().then(function () {
        assert.deepStrictEqual(calls.sort(), ['backends', 'downloads'],
            'во время ввода нельзя перерисовывать панель деталей (стирается набранное): ' + calls.join(','));
    });
});

check('фокус вне панели деталей обновление не блокирует', function () {
    const calls = [];
    fakeModule(calls);
    detailPanel._contains = null; // панель не содержит этот элемент
    global.document.activeElement = { tagName: 'INPUT' };
    assert.strictEqual(P._isTypingInDetail(), false, 'фокус вне панели — не ввод');
    return runProvider().then(function () {
        assert.deepStrictEqual(calls.sort(), ['backends', 'detail', 'downloads'],
            'без ввода провайдер обязан обновить и детали: ' + calls.join(','));
    });
});

check('кнопка/чекбокс внутри панели не считаются вводом текста', function () {
    const button = { tagName: 'BUTTON' };
    detailPanel._contains = button;
    global.document.activeElement = button;
    assert.strictEqual(P._isTypingInDetail(), false,
        'фокус на кнопке не должен блокировать обновление панели');
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
})();
