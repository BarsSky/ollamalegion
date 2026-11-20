// renderers-pages-split-r649.test.js — R649 (2026-11-20): страж выноса пяти
// страниц («Бэкенды», «Модели», «Сессии», «Агенты», «GGUF») из renderers.js
// в renderers-pages.js (фабрика).
//
// ЗАЧЕМ. Вынос держится на тех же трёх вещах, что R643, и все три уже ломались:
//   1. ПОРЯДОК. Фабрика вызывается при загрузке renderers.js, значит
//      renderers-pages.js обязан быть подключён раньше — иначе TypeError.
//   2. ПОЛНЫЙ СПИСОК ЗАВИСИМОСТЕЙ. Слой берёт 23 имени из renderers.js, включая
//      приходящие из деструктуризации Utils (`escapeHtml`, `getBackendTypeBadge`),
//      локальный `_t` и функции, объявленные НИЖЕ точки вызова (`modelsGrid`,
//      `logs`, `renderGgufCardsInto` — они поднимаются, но переменная-ссылка
//      передаётся сейчас). Список посчитан scripts/webui_extract_deps.js.
//   3. ОТСУТСТВИЕ КОПИЙ. Если функция снова определена в renderers.js, разрез
//      откатили, но старую копию забыли.
//
// Запуск: node webui/tests/renderers-pages-split-r649.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
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

const ROOT = path.join(__dirname, '..', '..');
const read = (p) => fs.readFileSync(path.join(ROOT, p), 'utf8');

const HTML = read('webui/index.html');
const RENDERERS = read('webui/js/modules/renderers.js');
const PAGES = read('webui/js/modules/renderers-pages.js');

const MOVED = ['backendsPage', 'modelsPage', 'sessionsPage', 'agentsPage', 'ggufPage'];
const REQUIRED_DEPS = ['Utils', '_t', 'backendLoad', 'badge', 'computeMemorySplit', 'dedupModelCards',
    'donorBadgeHtml', 'donorDetailHtml', 'emptyRow', 'escapeHtml', 'formatUptime', 'getBackendTypeBadge',
    'getClientIcon', 'loading', 'logs', 'modelsGrid', 'parseQuantizationFromName', 'protocolBadge',
    'renderGgufCardsInto', 'renderOllamaParams', 'stableBackendOrder', 'statusBadge', 'statusLabel'];

check('R649: renderers-pages.js подключён в index.html', function () {
    assert.ok(HTML.indexOf('js/modules/renderers-pages.js') > 0,
        'нет <script src="js/modules/renderers-pages.js">');
});

check('R649: страницы грузятся ПЕРЕД renderers.js', function () {
    const pagesAt = HTML.indexOf('src="js/modules/renderers-pages.js');
    const renderersAt = HTML.indexOf('src="js/modules/renderers.js');
    assert.ok(pagesAt > 0 && renderersAt > 0, 'оба <script> обязаны быть в index.html');
    assert.ok(pagesAt < renderersAt,
        'порядок обратный: renderers.js зовёт window.RenderersPages при загрузке и получит undefined');
});

check('R649: фабрика вызывается и получает ВСЕ зависимости', function () {
    assert.ok(/window\.RenderersPages\(\{/.test(RENDERERS), 'renderers.js не вызывает window.RenderersPages');
    REQUIRED_DEPS.forEach(function (dep) {
        assert.ok(new RegExp('\\b' + dep + '\\s*:\\s*' + dep + '\\b').test(RENDERERS),
            'в контекст фабрики не передан ' + dep + ' — упадёт ReferenceError при первом рендере');
        assert.ok(new RegExp('var\\s+' + dep + '\\s*=\\s*ctx\\.' + dep + '\\s*;').test(PAGES),
            'в renderers-pages.js не принят ctx.' + dep);
    });
});

check('R649: вынесенные функции больше НЕ определены в renderers.js', function () {
    MOVED.forEach(function (fn) {
        assert.ok(PAGES.indexOf('function ' + fn + '(') > 0, 'renderers-pages.js не содержит function ' + fn);
        assert.strictEqual(RENDERERS.indexOf('function ' + fn + '('), -1,
            fn + ' снова определён в renderers.js — копия разойдётся с вынесенной');
    });
});

check('R649: публичный объект Renderers не изменился', function () {
    assert.ok(/const Renderers = \(function \(\) \{/.test(RENDERERS), 'изменилась форма модуля Renderers');
    assert.ok(/window\.Renderers = Renderers;/.test(RENDERERS), 'пропал экспорт window.Renderers');
    MOVED.forEach(function (fn) {
        assert.ok(new RegExp('\\b' + fn + '\\b').test(RENDERERS),
            'Renderers API потерял ' + fn);
    });
});

check('R649: фабрика ВОЗВРАЩАЕТ пять функций (не только объявляет)', function () {
    // Текстовый чек выше не заметит `return { backendsPage }` без остальных.
    const saved = global.window;
    try {
        global.window = {};
        require(path.join(ROOT, 'webui/js/modules/renderers-pages.js'));
        assert.strictEqual(typeof global.window.RenderersPages, 'function',
            'renderers-pages.js не выставил window.RenderersPages');
        const stub = {};
        REQUIRED_DEPS.forEach(function (d) { stub[d] = function () {}; });
        const api = global.window.RenderersPages(stub);
        MOVED.forEach(function (fn) {
            assert.strictEqual(typeof api[fn], 'function', 'фабрика не вернула ' + fn);
        });
    } finally {
        global.window = saved;
    }
});

if (failures.length) {
    console.error('\nrenderers-pages-split-r649: ' + failures.length + ' провал(ов)');
    process.exit(1);
}
console.log('\nOK: ' + passed + ' проверок (R649)');
