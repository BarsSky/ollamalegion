// renderers-cluster-split-r651.test.js — R651 (2026-11-20): страж выноса дашборда,
// GPU-кластера, ёмкости, «доступных моделей» и таблицы бэкендов из renderers.js
// в renderers-cluster.js (фабрика).
//
// ЗАЧЕМ. Тот же класс, что R643/R649/R650: порядок, полный список зависимостей,
// отсутствие копий. Здесь у разреза два своих риска:
//   1. ПОРЯДОК. Фабрика кластера идёт ПОСЛЕ фабрики журналов: дашборд зовёт
//      queueView из неё. Перепутать — значит показать на дашборде пустую очередь.
//   2. СОСТОЯНИЕ. _predCache + пороги прогноза принадлежали ТОЛЬКО этой группе;
//      они переехали вместе с ней. Если _predCache снова появится в renderers.js,
//      прогноз будет жить в двух местах и гистерезис разъедется.
//
// Запуск: node webui/tests/renderers-cluster-split-r651.test.js
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
const CLUSTER = read('webui/js/modules/renderers-cluster.js');

const MOVED = ['dashboard', 'runtimeCluster', 'gpuCluster'];
const REQUIRED_DEPS = ['Utils', '_t', 'computeMemorySplit', 'donorBadgeHtml', 'emptyRow', 'escapeHtml',
    'formatMB', 'formatNumber', 'getBackendMode', 'getBackendModeBadge', 'getBackendTypeBadge',
    'getGPUStatus', 'getProgressClass', 'isZeroTimestamp', 'loading', 'parseQuantizationFromName',
    'percent', 'queueView', 'stableBackendOrder', 'statusBadge'];

check('R651: renderers-cluster.js подключён в index.html', function () {
    assert.ok(HTML.indexOf('js/modules/renderers-cluster.js') > 0,
        'нет <script src="js/modules/renderers-cluster.js">');
});

check('R651: кластер грузится ПЕРЕД renderers.js', function () {
    const clusterAt = HTML.indexOf('src="js/modules/renderers-cluster.js');
    const renderersAt = HTML.indexOf('src="js/modules/renderers.js');
    assert.ok(clusterAt > 0 && renderersAt > 0, 'оба <script> обязаны быть в index.html');
    assert.ok(clusterAt < renderersAt, 'порядок обратный: renderers.js зовёт window.RenderersCluster');
});

check('R651: фабрика вызывается и получает ВСЕ зависимости', function () {
    assert.ok(/window\.RenderersCluster\(\{/.test(RENDERERS), 'renderers.js не вызывает window.RenderersCluster');
    REQUIRED_DEPS.forEach(function (dep) {
        assert.ok(new RegExp('\\b' + dep + '\\s*:\\s*' + dep + '\\b').test(RENDERERS),
            'в контекст фабрики не передан ' + dep);
        assert.ok(new RegExp('var\\s+' + dep + '\\s*=\\s*ctx\\.' + dep + '\\s*;').test(CLUSTER),
            'в renderers-cluster.js не принят ctx.' + dep);
    });
});

check('R651: кластер строится ПОСЛЕ журналов (дашборд зовёт queueView)', function () {
    const logsAt = RENDERERS.indexOf('window.RenderersLogs({');
    const clusterAt = RENDERERS.indexOf('window.RenderersCluster({');
    assert.ok(logsAt > 0 && clusterAt > 0, 'оба вызова обязаны быть');
    assert.ok(logsAt < clusterAt,
        'фабрика кластера должна вызываться после журналов: дашборд берёт queueView из них');
    assert.ok(/queueView\s*:\s*queueView/.test(RENDERERS), 'queueView не передан в кластер');
});

check('R651: вынесенные функции больше НЕ определены в renderers.js', function () {
    MOVED.forEach(function (fn) {
        assert.ok(CLUSTER.indexOf('function ' + fn + '(') > 0, 'renderers-cluster.js не содержит function ' + fn);
        assert.strictEqual(RENDERERS.indexOf('function ' + fn + '('), -1, fn + ' снова определён в renderers.js');
    });
    ['backendsTable', 'capacitySection', 'capacityCard', 'availableModels', 'gpuCard', 'metric', 'tooltipRow', 'fmtExpires']
        .forEach(function (fn) {
            assert.ok(CLUSTER.indexOf('function ' + fn + '(') > 0, 'в кластер не переехала ' + fn);
        });
});

check('R651: состояние прогноза нагрузки переехало целиком', function () {
    assert.ok(/_predCache/.test(CLUSTER), 'в кластере нет _predCache — прогноз не работает');
    assert.strictEqual(RENDERERS.indexOf('_predCache'), -1,
        '_predCache снова в renderers.js: прогноз живёт в двух местах, гистерезис разъедется');
    assert.strictEqual(RENDERERS.indexOf('PRED_HYSTERESIS'), -1, 'PRED_HYSTERESIS остался в renderers.js');
    assert.ok(/var PRED_HYSTERESIS = 45;/.test(CLUSTER), 'порог гистерезиса 45 с потерян');
    assert.ok(/var PRED_THRESHOLD = 300;/.test(CLUSTER), 'порог прогноза 300 с потерян');
});

check('R651: публичный объект Renderers не изменился', function () {
    assert.ok(/const Renderers = \(function \(\) \{/.test(RENDERERS), 'изменилась форма модуля Renderers');
    assert.ok(/window\.Renderers = Renderers;/.test(RENDERERS), 'пропал экспорт window.Renderers');
    MOVED.forEach(function (fn) {
        assert.ok(new RegExp('\\b' + fn + '\\b').test(RENDERERS), 'Renderers API потерял ' + fn);
    });
});

check('R651: renderers.js ушёл ниже порога §5 (< 1500 строк)', function () {
    const lines = RENDERERS.split('\n').length;
    assert.ok(lines < 1500, 'renderers.js снова вырос до ' + lines + ' строк (цель < 1500)');
});

check('R651: фабрика ВОЗВРАЩАЕТ три функции (не только объявляет)', function () {
    const saved = global.window;
    try {
        global.window = {};
        require(path.join(ROOT, 'webui/js/modules/renderers-cluster.js'));
        assert.strictEqual(typeof global.window.RenderersCluster, 'function',
            'renderers-cluster.js не выставил window.RenderersCluster');
        const stub = {};
        REQUIRED_DEPS.forEach(function (d) { stub[d] = function () {}; });
        const api = global.window.RenderersCluster(stub);
        MOVED.forEach(function (fn) {
            assert.strictEqual(typeof api[fn], 'function', 'фабрика не вернула ' + fn);
        });
    } finally {
        global.window = saved;
    }
});

if (failures.length) {
    console.error('\nrenderers-cluster-split-r651: ' + failures.length + ' провал(ов)');
    process.exit(1);
}
console.log('\nOK: ' + passed + ' проверок (R651)');
