// renderers-params-split-r643.test.js — R643 (2026-11-19): страж выноса рендера
// параметров загрузки из renderers.js в renderers-params.js (фабрика).
//
// ЗАЧЕМ. Вынос держится на трёх вещах, и все три ломались на первом заходе:
//   1. ПОРЯДОК. Фабрика вызывается при загрузке renderers.js, значит
//      renderers-params.js обязан быть подключён раньше — иначе `undefined`.
//   2. ПОЛНЫЙ СПИСОК ЗАВИСИМОСТЕЙ. Слой берёт из renderers.js не только `Utils`,
//      но и функции деструктуризации (`escapeHtml`, `formatNumber`, `formatMB`,
//      `percent`), локальный `_t` и таблицу `OLLAMA_PARAMS`. Пропуск любой из них
//      падает ReferenceError-ом при первом рендере: так и было — сначала
//      `escapeHtml is not defined`, потом `OLLAMA_PARAMS is not defined`.
//   3. ОТСУТСТВИЕ КОПИЙ. Если функция снова определена в renderers.js, разрез
//      откатили, но старую копию забыли.
//
// Запуск: node webui/tests/renderers-params-split-r643.test.js
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
const PARAMS = read('webui/js/modules/renderers-params.js');

const MOVED = ['renderImageParams', 'formatAgeR348', 'formatLoadedAtR346', 'renderOllamaParams'];
const REQUIRED_DEPS = ['Utils', 'isZeroTimestamp', 'escapeHtml', 'formatNumber', 'formatMB', 'percent', '_t', 'OLLAMA_PARAMS'];

check('R643: renderers-params.js подключён в index.html', function () {
    assert.ok(HTML.indexOf('js/modules/renderers-params.js') > 0,
        'нет <script src="js/modules/renderers-params.js">');
});

check('R643: параметры грузятся ПЕРЕД renderers.js', function () {
    const paramsAt = HTML.indexOf('src="js/modules/renderers-params.js');
    const renderersAt = HTML.indexOf('src="js/modules/renderers.js');
    assert.ok(paramsAt > 0 && renderersAt > 0, 'оба <script> обязаны быть в index.html');
    assert.ok(paramsAt < renderersAt,
        'порядок обратный: renderers.js зовёт window.RenderersParams при загрузке и получит undefined');
});

check('R643: фабрика вызывается и получает ВСЕ зависимости', function () {
    assert.ok(/window\.RenderersParams\(\{/.test(RENDERERS), 'renderers.js не вызывает window.RenderersParams');
    REQUIRED_DEPS.forEach(function (dep) {
        assert.ok(new RegExp('\\b' + dep + '\\s*:\\s*' + dep.replace('$', '\\$') + '\\b').test(RENDERERS),
            'в контекст фабрики не передан ' + dep + ' — упадёт ReferenceError при первом рендере');
        assert.ok(new RegExp('var\\s+' + dep + '\\s*=\\s*ctx\\.' + dep + '\\s*;').test(PARAMS),
            'в renderers-params.js не принят ctx.' + dep);
    });
});

check('R643: вынесенные функции больше НЕ определены в renderers.js', function () {
    MOVED.forEach(function (fn) {
        assert.ok(PARAMS.indexOf('function ' + fn + '(') > 0, 'renderers-params.js не содержит function ' + fn);
        assert.strictEqual(RENDERERS.indexOf('function ' + fn + '('), -1,
            fn + ' снова определён в renderers.js — копия разойдётся с вынесенной');
    });
});

check('R643: публичный объект Renderers не изменился', function () {
    // Один и тот же экспорт: const Renderers = … ; window.Renderers = Renderers;
    assert.ok(/const Renderers = \(function \(\) \{/.test(RENDERERS), 'изменилась форма модуля Renderers');
    assert.ok(/window\.Renderers = Renderers;/.test(RENDERERS), 'пропал экспорт window.Renderers');
    ['renderImageParams', 'renderOllamaParams'].forEach(function (fn) {
        assert.ok(new RegExp('\\b' + fn + '\\b').test(RENDERERS),
            'Renderers API потерял ' + fn);
    });
});

if (failures.length) {
    console.error('\nrenderers-params-split-r643: ' + failures.length + ' провал(ов)');
    process.exit(1);
}
console.log('\nOK: ' + passed + ' проверок (R643)');
