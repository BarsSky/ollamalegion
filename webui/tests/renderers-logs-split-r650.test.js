// renderers-logs-split-r650.test.js — R650 (2026-11-20): страж выноса очереди,
// журналов событий и прокси-логов из renderers.js в renderers-logs.js (фабрика).
//
// ЗАЧЕМ. Тот же класс, что R643/R649: порядок подключения, полный список
// зависимостей и отсутствие копий. У этого слоя есть своя ловушка — он вынесен
// ПЕРВЫМ по вызову: renderers.js строит его фабрику до фабрики страниц, потому что
// страницам нужен `logs`. Если порядок перепутать, `logs` уедет в контекст
// страниц как undefined, и упадёт только при открытии страницы «Логи».
//
// Запуск: node webui/tests/renderers-logs-split-r650.test.js
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
const LOGS = read('webui/js/modules/renderers-logs.js');

const MOVED = ['queueView', 'queuePage', 'logs', 'predictionAlerts', 'proxyLogs', 'copyProxyLogs'];
const REQUIRED_DEPS = ['Utils', '_t', 'percent', 'emptyRow', 'escapeAttr', 'escapeHtml', 'protocolBadge'];

check('R650: renderers-logs.js подключён в index.html', function () {
    assert.ok(HTML.indexOf('js/modules/renderers-logs.js') > 0,
        'нет <script src="js/modules/renderers-logs.js">');
});

check('R650: журналы грузятся ПЕРЕД renderers.js', function () {
    const logsAt = HTML.indexOf('src="js/modules/renderers-logs.js');
    const renderersAt = HTML.indexOf('src="js/modules/renderers.js');
    assert.ok(logsAt > 0 && renderersAt > 0, 'оба <script> обязаны быть в index.html');
    assert.ok(logsAt < renderersAt,
        'порядок обратный: renderers.js зовёт window.RenderersLogs при загрузке');
});

check('R650: фабрика вызывается и получает ВСЕ зависимости', function () {
    assert.ok(/window\.RenderersLogs\(\{/.test(RENDERERS), 'renderers.js не вызывает window.RenderersLogs');
    REQUIRED_DEPS.forEach(function (dep) {
        assert.ok(new RegExp('\\b' + dep + '\\s*:\\s*' + dep + '\\b').test(RENDERERS),
            'в контекст фабрики не передан ' + dep);
        assert.ok(new RegExp('var\\s+' + dep + '\\s*=\\s*ctx\\.' + dep + '\\s*;').test(LOGS),
            'в renderers-logs.js не принят ctx.' + dep);
    });
});

check('R650: журналы строятся ДО страниц (страницам нужен logs)', function () {
    const logsAt = RENDERERS.indexOf('window.RenderersLogs({');
    const pagesAt = RENDERERS.indexOf('window.RenderersPages({');
    assert.ok(logsAt > 0 && pagesAt > 0, 'оба вызова обязаны быть');
    assert.ok(logsAt < pagesAt,
        'фабрика журналов должна вызываться раньше страниц: страницы получают logs из неё');
});

check('R650: вынесенные функции больше НЕ определены в renderers.js', function () {
    MOVED.forEach(function (fn) {
        assert.ok(LOGS.indexOf('function ' + fn + '(') > 0, 'renderers-logs.js не содержит function ' + fn);
        assert.strictEqual(RENDERERS.indexOf('function ' + fn + '('), -1,
            fn + ' снова определён в renderers.js — копия разойдётся с вынесенной');
    });
});

check('R650: дашборд по-прежнему зовёт queueView (связка не потеряна)', function () {
    // R651: dashboard переехал в renderers-cluster.js — проверяем связку там и то,
    // что queueView доехал до фабрики кластера из фабрики журналов.
    const CLUSTER = read('webui/js/modules/renderers-cluster.js');
    assert.ok(/queueView\(queue\)/.test(CLUSTER),
        'dashboard перестал звать queueView — очередь на дашборде опустеет');
    assert.ok(/queueView\s*:\s*queueView/.test(RENDERERS),
        'в контекст фабрики кластера не передан queueView');
    assert.ok(/var\s+queueView\s*=\s*logRenderers\.queueView/.test(RENDERERS),
        'renderers.js не присвоил queueView из фабрики журналов');
});

check('R650: публичный объект Renderers не изменился', function () {
    assert.ok(/const Renderers = \(function \(\) \{/.test(RENDERERS), 'изменилась форма модуля Renderers');
    assert.ok(/window\.Renderers = Renderers;/.test(RENDERERS), 'пропал экспорт window.Renderers');
    MOVED.forEach(function (fn) {
        assert.ok(new RegExp('\\b' + fn + '\\b').test(RENDERERS), 'Renderers API потерял ' + fn);
    });
});

check('R650: фабрика ВОЗВРАЩАЕТ шесть функций (не только объявляет)', function () {
    const saved = global.window;
    try {
        global.window = {};
        require(path.join(ROOT, 'webui/js/modules/renderers-logs.js'));
        assert.strictEqual(typeof global.window.RenderersLogs, 'function',
            'renderers-logs.js не выставил window.RenderersLogs');
        const stub = {};
        REQUIRED_DEPS.forEach(function (d) { stub[d] = function () {}; });
        const api = global.window.RenderersLogs(stub);
        MOVED.forEach(function (fn) {
            assert.strictEqual(typeof api[fn], 'function', 'фабрика не вернула ' + fn);
        });
    } finally {
        global.window = saved;
    }
});

if (failures.length) {
    console.error('\nrenderers-logs-split-r650: ' + failures.length + ' провал(ов)');
    process.exit(1);
}
console.log('\nOK: ' + passed + ' проверок (R650)');
