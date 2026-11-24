// reexport-panel-tab-r746.test.js — R746: блок ре-экспорта живёт ВКЛАДКОЙ
// бэкенда, а не отдельным блоком над страницей; выбор модели виден и работает.
//
// ЖИВЫЕ ПРЕТЕНЗИИ, ЗАКРЕПЛЁННЫЕ ЗДЕСЬ:
//   1. «нет ясности, что к чему относится» — блок висел над страницей GGUF, вне
//      контекста выбранного бэкенда. Теперь это вкладка `reexport` в панели
//      бэкенда (рядом с «Модели»), и она появляется только при живом модуле.
//   2. «не понятно, как выбирать модель» — строки таблицы выбирались кликом, но
//      это было видно только по фону. Теперь есть радиокнопка в колонке выбора и
//      строка «выбрано: файл».
//   3. «нет выбора бэкенда» — бэкенд определяется вкладкой (он выбран слева), и
//      шаг 5 называет узел проверки текстом.
//
// Тест статический: он проверяет связку файлов (рендерер → вкладка → монтирование
// блока → i18n). Поведение в браузере проверяется живым прогоном на стенде.
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
        console.error('  \u2717 ' + name + ' — ' + (e && e.message));
    }
}

const ROOT = path.join(__dirname, '..', '..');
const read = (rel) => fs.readFileSync(path.join(ROOT, rel), 'utf8');
const detailRender = read('webui/js/modules/gguf-renderer-detail-render.js');
const renderer = read('webui/js/modules/gguf-renderer.js');
const refresh = read('webui/js/modules/gguf-renderer-refresh.js');
const module_ = read('webui/js/modules/gguf-reexport.js');
const helpLayer = read('webui/js/modules/gguf-reexport-help.js');
const indexHtml = read('webui/index.html');
const ru = read('webui/js/i18n/ru.js');
const en = read('webui/js/i18n/en.js');

check('вкладка reexport объявлена и показывается только при живом модуле', function () {
    assert.ok(/\{ id: 'reexport'/.test(detailRender), 'нет вкладки id: reexport в renderDetailTabs');
    assert.ok(/GgufReexport\.isAvailable\(\)/.test(detailRender),
        'вкладка не фильтруется по доступности модуля — на стенде без профиля reexport появится пустая вкладка');
    assert.ok(/case 'reexport':\s*return '<div id="ggufReexportHost"><\/div>';/.test(detailRender),
        'контент вкладки reexport не создаёт хост #ggufReexportHost');
});

check('блок больше не висит над всей страницей GGUF', function () {
    assert.ok(renderer.indexOf("'<div id=\"ggufReexportHost\"></div>' +") < 0,
        'остался page-level хост ре-экспорта — будет два хоста и неясная привязка к бэкенду');
    assert.ok(/GgufReexport\.probe\(\)/.test(renderer),
        'страница не пробует модуль — вкладка не появится даже при поднятом сервисе');
});

check('панель монтирует блок при активной вкладке reexport', function () {
    assert.ok(/mountReexportIfPaneR746/.test(refresh), 'нет вызова mountReexportIfPaneR746');
    assert.ok(/detailPane !== 'reexport'/.test(refresh), 'монтирование не привязано к активной вкладке reexport');
    assert.ok(/Re\.mount\(\)/.test(refresh), 'блок не монтируется после перерисовки панели');
});

check('модуль отдаёт isAvailable/probe и помнит раскрытие справки', function () {
    assert.ok(/isAvailable: function \(\)/.test(module_), 'нет экспорта isAvailable (нужен фильтру вкладки)');
    assert.ok(/probe: function \(\)/.test(module_), 'нет экспорта probe (нужен странице для пробы)');
    assert.ok(/helpOpen: false/.test(module_), 'состояние справки не хранится в модуле — <details> будет схлопываться');
});

check('выбор модели виден: радиокнопка и строка «выбрано»', function () {
    assert.ok(/rx-col-pick/.test(module_), 'нет колонки выбора в таблице моделей');
    assert.ok(/name="rxModelPick"/.test(module_), 'нет радиокнопки выбора модели');
    assert.ok(/id="rxSelected"/.test(module_), 'нет строки «выбрано: файл»');
});

check('слой пояснений подключён ДО главного модуля', function () {
    const iHelp = indexHtml.indexOf('gguf-reexport-help.js');
    const iMain = indexHtml.indexOf('gguf-reexport.js');
    assert.ok(iHelp > 0, 'нет script-тега gguf-reexport-help.js');
    assert.ok(iMain > iHelp, 'слой пояснений подключён после главного модуля — helpLayer его не увидит');
    assert.ok(/GgufReexportHelp/.test(helpLayer) && /function helpHTML/.test(helpLayer),
        'слой пояснений не экспортирует фабрику/справку');
});

check('i18n: новые ключи есть в ru и en (симметрично)', function () {
    const keys = ['gguf.tab_reexport', 'reexport.col_pick', 'reexport.selected_prefix',
        'reexport.selected_none', 'reexport.load_node_pre', 'reexport.load_node_missing',
        'reexport.scope_note'];
    keys.forEach(function (k) {
        assert.ok(ru.indexOf('"' + k + '"') >= 0, 'нет ' + k + ' в ru.js');
        assert.ok(en.indexOf('"' + k + '"') >= 0, 'нет ' + k + ' в en.js');
    });
});

if (failures.length) {
    console.error('\nFAIL: ' + failures.length + ' красных, passed=' + passed);
    process.exit(1);
}
console.log('\nOK: ' + passed + ' проверок (R746)');
