// monitor-panels-split-r641.test.js — R641 (2026-11-19): страж разреза
// webui/js/monitor/ui-renderer.js на «рендерер страницы» и «панели».
//
// ЗАЧЕМ. Разрез держится на трёх вещах, каждая из которых ломается молча:
//   1. ПОРЯДОК ЗАГРУЗКИ. `ui-renderer.js` берёт `window.MonitorUiPanels` и раздаёт
//      имена локальными ссылками. Переставили <script> или забыли подключить новый
//      файл — в браузере это будет «панели пустые», а не ошибка сборки.
//   2. ПОЛНОТА ЭКСПОРТА. Первая версия выноса экспортировала только 5 имён, а
//      `renderBackends` продолжал звать `imagePortOf`/`imageModelsOf`/`imageStateText`
//      из вынесенной группы — смоук покраснел 17 проверками («image: порт воркера не
//      виден», «бейдж типа не отрисован»). Здесь список имён сверяется механически:
//      столько же `var X = panels.X;` в рендерере, сколько в экспорте.
//   3. ОТСУТСТВИЕ КОПИЙ. Если функция снова определена в рендерере, разрез откатили,
//      но старую копию забыли — правки начнут расходиться.
//
// Запуск: node webui/tests/monitor-panels-split-r641.test.js
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

const MONITOR_HTML = read('webui/monitor.html');
const RENDERER = read('webui/js/monitor/ui-renderer.js');
const PANELS = read('webui/js/monitor/ui-renderer-panels.js');

// Вынесенные функции (19): помощники изображений, updateText и три панели.
const MOVED = [
    'imageBlockOf', 'imageModelsOf', 'imagePortOf', 'isImageBackendType', 'isImageBackend',
    'imageStateText', 'imageSizeBytesOf', 'fmtBytesShort', 'escHtml', 'fmtImageDuration',
    'fmtImageSize', 'fmtImageTime', 'imageRequestStatusText', 'imageRequestStatusBadge',
    'updateText', 'applyImageRequestsPanelI18n', 'renderImageRequests',
    'renderAutoPullPanel', 'renderVirtualModels'
];

check('R641: ui-renderer-panels.js подключён в monitor.html', function () {
    assert.ok(MONITOR_HTML.indexOf('js/monitor/ui-renderer-panels.js') > 0,
        'нет <script src="js/monitor/ui-renderer-panels.js">');
});

check('R641: панели грузятся ПЕРЕД рендерером', function () {
    const panelsAt = MONITOR_HTML.indexOf('src="js/monitor/ui-renderer-panels.js');
    const rendererAt = MONITOR_HTML.indexOf('src="js/monitor/ui-renderer.js');
    assert.ok(panelsAt > 0 && rendererAt > 0, 'оба <script> обязаны быть в monitor.html');
    assert.ok(panelsAt < rendererAt,
        'порядок обратный: рендерер берёт window.MonitorUiPanels и без него бросает исключение');
});

check('R641: рендерер громко падает без панелей', function () {
    assert.ok(RENDERER.indexOf('window.MonitorUiPanels') > 0, 'рендерер не читает window.MonitorUiPanels');
    assert.ok(/throw new Error\(/.test(RENDERER),
        'нет громкой ошибки при отсутствии панелей: страница будет «тихо пустой»');
});

check('R641: экспорт и алиасы совпадают по составу (19 имён)', function () {
    MOVED.forEach(function (fn) {
        assert.ok(new RegExp('\\b' + fn + '\\s*:\\s*' + fn + '\\b').test(PANELS),
            'панели не экспортируют ' + fn);
        assert.ok(new RegExp('var\\s+' + fn + '\\s*=\\s*panels\\.' + fn + '\\s*;').test(RENDERER),
            'рендерер не раздаёт локальную ссылку на ' + fn + ' — вызов из renderBackends упадёт молча');
    });
});

check('R641: вынесенные функции больше НЕ определены в рендерере', function () {
    MOVED.forEach(function (fn) {
        assert.ok(PANELS.indexOf('function ' + fn + '(') > 0, 'панели не содержат function ' + fn);
        assert.strictEqual(RENDERER.indexOf('function ' + fn + '('), -1,
            fn + ' снова определён в рендерере — копия разойдётся с панелями');
    });
});

check('R641: публичные точки входа страницы не изменились', function () {
    ['window.updateUI = updateUI;', 'window.switchBackendType = switchBackendType;',
        'window.renderBackendTypeSwitcher = renderBackendTypeSwitcher;',
        'window.getMonitorBackendType = effectiveMonitorType;'].forEach(function (line) {
            assert.ok(RENDERER.indexOf(line) > 0, 'пропал экспорт: ' + line);
        });
});

check('R641: панели не тянут состояние страницы напрямую', function () {
    // Панели получают данные аргументами; им разрешён только MonitorApp (T/esc/fmtDur).
    assert.strictEqual(PANELS.indexOf('MonitorState'), -1,
        'панели полезли в MonitorState: разрез перестанет быть «по данным снаружи»');
});

if (failures.length) {
    console.error('\nmonitor-panels-split-r641: ' + failures.length + ' провал(ов)');
    process.exit(1);
}
console.log('\nOK: ' + passed + ' проверок (R641)');
