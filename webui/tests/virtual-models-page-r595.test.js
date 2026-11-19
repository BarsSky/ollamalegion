// virtual-models-page-r595.test.js — R595 (2026-11-19): «Виртуальные модели» в SPA.
//
// Запуск: node webui/tests/virtual-models-page-r595.test.js
//
// ЖИВОЙ КОНТЕКСТ. До R595 CRUD виртуальных моделей жил в отдельном файле
// `webui/virtual-models.html` (547 строк), который открывался в новой вкладке и
// обрабатывался браузером, а не SPA. Из этого следовало три дефекта, а не только
// «некрасиво»:
//   1. СВОЯ авторизация: X-API-Token из WEBUI_CONFIG/localStorage и НИЧЕГО про
//      X-CSRF-Token (R202: мутирующие запросы через сессию требуют CSRF) → Create
//      и Delete на стенде с сессионной авторизацией отбивались;
//   2. три места в коде знали про страницу спецслучаем (navModeIndependent,
//      регулярка «внешние html» в demo-mode.js, ветка в app-listeners.js), а
//      ссылка ?page=virtual-models не работала вовсе;
//   3. ни одного i18n-ключа: тексты жёстко английские.
//
// Что проверяется:
//   1. тело POST собирается по прежним правилам (trim, пул по строке, timeout);
//   2. DELETE-адрес кодирует имя как сегмент пути;
//   3. строки таблицы экранируют всё, что пришло с сервера;
//   4. сводка считает модели и выборы бэкенда;
//   5. источник: отдельного файла больше нет, пункт меню — SPA-ссылка без
//      target=_blank, спецслучаи сняты, модуль подключён.

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const ROOT = path.join(__dirname, '..', '..');
const MODULE = path.join(ROOT, 'webui', 'js', 'modules', 'virtual-models-page.js');
const INDEX = path.join(ROOT, 'webui', 'index.html');
const OLD_PAGE = path.join(ROOT, 'webui', 'virtual-models.html');

global.window = global;
global.I18N = { t: function (key, vars) { return 'I18N:' + key; } };
global.document = {
    getElementById: function () { return null; },
    querySelectorAll: function () { return []; },
    addEventListener: function () {}
};

const NodeModule = require('module');
const m = new NodeModule(MODULE, module);
m._compile(fs.readFileSync(MODULE, 'utf8'), MODULE);
const VM = global.window.VirtualModelsPage;

let checks = 0;
const failures = [];
function check(name, fn) {
    try { fn(); checks++; console.log('  ✓ ' + name); } catch (e) { failures.push(name + ' — ' + e.message); console.error('  ✗ ' + name + ' — ' + e.message); }
}

check('R595: тело POST собирается по прежним правилам', function () {
    const body = VM.buildPayload({
        name: '  virtual:my-gpt  ',
        description: '  Dist Llama  ',
        modelName: ' llama-70b ',
        selection: 'random',
        backendPool: ' backend-1:11434 \n\n backend-2:11434\n',
        timeoutMs: '45000'
    });
    assert.strictEqual(body.name, 'virtual:my-gpt');
    assert.strictEqual(body.description, 'Dist Llama');
    assert.strictEqual(body.modelName, 'llama-70b');
    assert.strictEqual(body.selection, 'random');
    assert.deepStrictEqual(body.backendPool, ['backend-1:11434', 'backend-2:11434'],
        'пул разбирается по строке, пустые строки выбрасываются');
    assert.strictEqual(body.timeoutMs, 45000);
});

check('R595: пустой/мусорный timeout → 0 (значит «по умолчанию»), selection по умолчанию round_robin', function () {
    const body = VM.buildPayload({ name: 'v', modelName: 'm', backendPool: 'b', timeoutMs: 'abc' });
    assert.strictEqual(body.timeoutMs, 0);
    assert.strictEqual(body.selection, 'round_robin');
    assert.deepStrictEqual(VM.buildPayload(null).backendPool, [], 'null-вход не падает');
});

check('R595: DELETE-адрес кодирует имя как сегмент пути', function () {
    assert.strictEqual(VM.deleteUrl('virtual:my gpt'), '/api/v1/virtual-models/virtual%3Amy%20gpt');
    assert.strictEqual(VM.deleteUrl('a/b'), '/api/v1/virtual-models/a%2Fb',
        'слэш в имени не должен создавать лишний сегмент пути');
});

check('R595: строки таблицы экранируют данные сервера', function () {
    const html = VM.rowsHtml([{
        name: '<img src=x onerror=alert(1)>',
        modelName: '"quoted"',
        description: '<b>x</b>',
        backendPool: ['<script>'],
        selections: { '<i>': 3 }
    }]);
    assert.ok(html.indexOf('<img') === -1, 'не экранировано имя: ' + html);
    assert.ok(html.indexOf('<script>') === -1, 'не экранирован бэкенд: ' + html);
    assert.ok(html.indexOf('&lt;img') !== -1, 'экранирование имени потеряно');
    assert.ok(/vm-delete/.test(html), 'нет кнопки удаления');
});

check('R595: пустой список — понятная подсказка, а не пустая таблица', function () {
    const html = VM.rowsHtml([]);
    assert.ok(/empty-state/.test(html), 'нет пустого состояния');
    assert.ok(html.indexOf('<table') === -1, 'нарисована таблица без строк');
});

check('R595: сводка считает модели и выборы', function () {
    const s = VM.summary([
        { selections: { a: 2, b: 3 } },
        { selections: { c: 1 } },
        {}
    ]);
    assert.strictEqual(s.count, 3);
    assert.strictEqual(s.selections, 6);
    assert.deepStrictEqual(VM.summary(null), { count: 0, selections: 0 });
});

check('R595: отдельного virtual-models.html больше нет', function () {
    assert.strictEqual(fs.existsSync(OLD_PAGE), false,
        'файл вернулся: страница снова живёт вне SPA (своя авторизация без CSRF)');
});

check('R595: пункт меню — SPA-ссылка без target=_blank, страница подключена', function () {
    const html = fs.readFileSync(INDEX, 'utf8');
    const nav = html.match(/<a[^>]*data-page="virtual-models"[^>]*>/);
    assert.ok(nav, 'пункт меню не найден');
    assert.ok(/href="#virtual-models"/.test(nav[0]), 'пункт меню ведёт не в SPA: ' + nav[0]);
    assert.ok(!/target="_blank"/.test(nav[0]), 'остался target="_blank": ' + nav[0]);
    assert.ok(/id="virtual-models-page"/.test(html), 'нет SPA-секции #virtual-models-page');
    assert.ok(/js\/modules\/virtual-models-page\.js/.test(html), 'модуль не подключён');
});

check('R595: спецслучаи сняты (navModeIndependent, demo-mode)', function () {
    const settingsUi = fs.readFileSync(path.join(ROOT, 'webui', 'js', 'modules', 'settings-ui.js'), 'utf8');
    assert.ok(!/navModeIndependent = \[[^\]]*'virtual-models'/.test(settingsUi),
        'virtual-models снова в navModeIndependent: страница опять «особая»');
    const demo = fs.readFileSync(path.join(ROOT, 'webui', 'js', 'modules', 'demo-mode.js'), 'utf8');
    assert.ok(!/\|virtual-models\)\\\.html/.test(demo),
        'virtual-models снова в регулярке «внешние html» demo-mode.js');
});

check('R595: страница в списке известных роутера и в refreshPage', function () {
    const app = fs.readFileSync(path.join(ROOT, 'webui', 'js', 'app.js'), 'utf8');
    assert.ok(/'virtual-models', 'agents'/.test(app) || /'virtual-models'/.test(app),
        'virtual-models не в списке страниц pageFromURL');
    assert.ok(/case 'virtual-models':/.test(app), 'refreshPage не знает про страницу');
});

if (failures.length) {
    console.error('\nИТОГ: ' + failures.length + ' провалов из ' + (checks + failures.length));
    process.exit(1);
}
console.log('\n' + checks + ' проверок, все зелёные');
