// reference-page-split-r642.test.js — R642 (2026-11-19): страж разреза страницы
// «Справка и шаблоны» на «страницу» и «состояние + помощники».
//
// ЗАЧЕМ. Разрез держится на трёх вещах, и все три ломаются молча:
//   1. ПОРЯДОК ЗАГРУЗКИ. `reference-page.js` берёт `window.ReferencePageHelpers`;
//      переставили <script> — в браузере это «страница не отрисовалась», а не ошибка
//      сборки.
//   2. ОДИН ОБЪЕКТ СОСТОЯНИЯ. `state` экспортируется по ССЫЛКЕ. Если в помощниках
//      появится `state = {…}` (переприсваивание), страница останется со старым
//      объектом, и правки полей перестанут доходить — расхождение проявится как
//      «нажал, ничего не изменилось».
//   3. ПОЛНОТА ЭКСПОРТА И ОТСУТСТВИЕ КОПИЙ. Первая версия выноса забыла
//      `backendChoices` — страница упала с `ReferenceError: backendChoices is not
//      defined` на первом же require. Здесь состав сверяется механически.
//
// Запуск: node webui/tests/reference-page-split-r642.test.js
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

// index.html лежит в webui/, а не в корне репозитория (R642: поймано первым прогоном).
const ROOT = path.join(__dirname, '..', '..');
const read = (p) => fs.readFileSync(path.join(ROOT, p), 'utf8');

const HTML = read('webui/index.html');
const PAGE = read('webui/js/modules/reference-page.js');
const HELPERS = read('webui/js/modules/reference-page-helpers.js');

// Имена, перенесённые в помощники (34 функции + состояние).
const MOVED = ['t', 'esc', 'byId', 'api', 'modelsApi', 'paramSpecs', 'paramSpec', 'specOrder',
    'specLabel', 'formatParamValue', 'formatDiffValue', 'formatDiffRow', 'renderDiff', 'globMatch',
    'matchRecommendation', 'filterRecommendations', 'appliesToChips', 'loadedTargetsFor',
    'normalizeModelName', 'applyGate', 'gateText', 'errorInfo', 'errorText', 'errorHtml',
    'paramRows', 'renderParamsTable', 'sourcesHtml', 'recMetaHtml', 'renderRecommendationRow',
    'renderRecommendationList', 'templateById', 'modelChoices', 'backendChoices', 'catalogModelNames'];

check('R642: reference-page-helpers.js подключён в index.html', function () {
    assert.ok(HTML.indexOf('js/modules/reference-page-helpers.js') > 0,
        'нет <script src="js/modules/reference-page-helpers.js">');
});

check('R642: помощники грузятся ПЕРЕД страницей', function () {
    const helpersAt = HTML.indexOf('src="js/modules/reference-page-helpers.js');
    const pageAt = HTML.indexOf('src="js/modules/reference-page.js');
    assert.ok(helpersAt > 0 && pageAt > 0, 'оба <script> обязаны быть в index.html');
    assert.ok(helpersAt < pageAt,
        'порядок обратный: страница берёт window.ReferencePageHelpers и без него падает');
});

check('R642: страница громко падает без помощников', function () {
    assert.ok(PAGE.indexOf('window.ReferencePageHelpers') > 0, 'страница не читает window.ReferencePageHelpers');
    assert.ok(/throw new Error\(/.test(PAGE),
        'нет громкой ошибки при отсутствии помощников: страница будет «тихо пустой»');
});

check('R642: экспорт и алиасы совпадают по составу (' + MOVED.length + ' имён)', function () {
    MOVED.forEach(function (fn) {
        assert.ok(new RegExp('\\b' + fn + '\\s*:\\s*' + fn + '\\b').test(HELPERS),
            'помощники не экспортируют ' + fn + ' (это уже падало: ReferenceError при загрузке)');
        assert.ok(new RegExp('var\\s+' + fn + '\\s*=\\s*helpers\\.' + fn + '\\s*;').test(PAGE),
            'страница не раздаёт локальную ссылку на ' + fn);
    });
});

check('R642: state экспортируется по ССЫЛКЕ и не переприсваивается', function () {
    assert.ok(/window\.ReferencePageHelpers\s*=\s*\{[\s\S]*?\bstate:\s*state\b/.test(HELPERS),
        'state не экспортирован из помощников');
    assert.ok(/var\s+state\s*=\s*helpers\.state\s*;/.test(PAGE),
        'страница не берёт state из помощников');
    // Переприсваивание в любом из двух файлов сломало бы общий объект молча.
    [['helpers', HELPERS], ['page', PAGE]].forEach(function (pair) {
        const hits = (pair[1].match(/^\s*state\s*=[^=]/gm) || []).length;
        assert.strictEqual(hits, 0, 'в ' + pair[0] + ' появилось переприсваивание state = … (объект разойдётся)');
    });
});

check('R642: вынесенные функции больше НЕ определены на странице', function () {
    MOVED.forEach(function (fn) {
        assert.ok(HELPERS.indexOf('function ' + fn + '(') > 0, 'помощники не содержат function ' + fn);
        assert.strictEqual(PAGE.indexOf('function ' + fn + '('), -1,
            fn + ' снова определён на странице — копия разойдётся с помощниками');
    });
});

check('R642: публичная точка входа страницы не изменилась', function () {
    // Считаем ТОЛЬКО назначение в коде: в шапке файла есть строка
    // «Экспорт: window.ReferencePage = { mount, load, ... }» — счёт «любых
    // вхождений» давал ложные 2 (поймано этим же тестом на первом прогоне).
    const count = (PAGE.match(/^\s*window\.ReferencePage = \{/gm) || []).length;
    assert.ok(count === 1, 'window.ReferencePage должен присваиваться ровно один раз, а не ' + count);
});

if (failures.length) {
    console.error('\nreference-page-split-r642: ' + failures.length + ' провал(ов)');
    process.exit(1);
}
console.log('\nOK: ' + passed + ' проверок (R642)');
