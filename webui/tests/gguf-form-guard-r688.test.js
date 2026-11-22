// webui/tests/gguf-form-guard-r688.test.js — R688 (2026-11-22): правая панель
// GGUF не стирает незавершённый ввод оператора при авто-обновлении.
//
// ЖИВАЯ ЖАЛОБА (дословно): «Форма у модели (в которой задаёшь параметры для
// загрузки) всё ещё скрывается и происходит постоянное обновление страницы gguf
// models, отчего нет возможности самому задать параметры через веб-интерфейс».
//
// ЗАМЕР НА СТЕНДЕ ДО ПРАВКИ (CDP, образ r684, страница GGUF → бэкенд → таб
// «Настройки» → форма параметров загрузки): 12 пересборок правой панели за 20 с
// (8 панели + 4 содержимого), раскрытая оператором секция «KV cache» схлопнулась,
// набранное 9999 заменено серверным 65536. При этом пока страница помечена
// «грязной», тик не обновлял данные ВООБЩЕ (0 запросов за 20 с) — то есть
// страница либо стоит, либо стирает форму.
//
// ЧТО ПРОВЕРЯЕТСЯ (поведенчески, на настоящих модулях):
//   1. набрал значение → ушёл из поля → ДВА цикла авто-прохода: панель не
//      пересобирается, значение цело, раскрытая секция остаётся раскрытой;
//   2. одна пересборка по ДРУГОЙ причине (смена бэкенда/таба) не теряет ни
//      значение оператора, ни раскрытые секции, а серверные значения соседних
//      полей при этом применяются (рендер реально произошёл);
//   3. сохранение снимает защиту: после noteSaved авто-проход снова применяет
//      серверные значения (форма остаётся актуальной);
//   4. защита НЕ вырождается в «панель не обновляется никогда»: при «грязной»
//      странице без правок в самой панели авто-проход пересобирает её;
//   5. статическая обвязка: страница GGUF в SAFE_FORM_PAGES (данные не глушатся),
//      провайдер передаёт {auto:true}, модуль защиты подключён в index.html ДО
//      refresh-модуля, сохранение (saveBackendOptions/applyModelParams) зовёт
//      noteSaved.
//
// ТЕСТ НЕ-ВАКУУМНЫЙ: на коде до R688 (git show HEAD:...) он КРАСНЕЕТ — там
// refreshDetailPane игнорирует аргументы и просто пишет innerHTML, поэтому
// набранное значение теряется на первом же авто-проходе.
//
// Запуск: node webui/tests/gguf-form-guard-r688.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const root = path.join(__dirname, '..');
const read = (rel) => fs.readFileSync(path.join(root, rel), 'utf8');

// --- фейковый DOM ------------------------------------------------------------
//
// Контейнер, чей innerHTML пересоздаёт поля и секции — ровно так, как это делает
// браузер. Этого достаточно: SafeForm/FormDirty/guard работают с узлами через
// id/value/open/querySelectorAll, а не через живой рендеринг.

function makeContainer(id, pageEl) {
    const box = {
        id: id,
        tagName: 'DIV',
        parentNode: pageEl,
        _details: [],
        _fields: [],
        _html: '',
        querySelectorAll: function (selector) {
            var sel = String(selector || '').trim();
            if (sel === 'details') return box._details.slice();
            if (sel === 'input[id], select[id], textarea[id]') return box._fields.slice();
            if (sel === '[data-form-key]') return box._fields.slice();
            return [];
        },
        querySelector: function () { return null; }
    };
    Object.defineProperty(box, 'innerHTML', {
        get: function () { return box._html; },
        set: function (html) {
            box._html = String(html);
            box._details = [];
            box._fields = [];
            var dRe = /<details([^>]*)>/g;
            var m;
            while ((m = dRe.exec(box._html)) !== null) {
                var attrs = attrsOf(m[1]);
                box._details.push({
                    tagName: 'DETAILS',
                    className: attrs['class'] || 'gguf-acc',
                    open: Object.prototype.hasOwnProperty.call(attrs, 'open'),
                    id: '',
                    parentNode: box,
                    _attrs: attrs,
                    getAttribute: function (n) {
                        return Object.prototype.hasOwnProperty.call(this._attrs, n) ? this._attrs[n] : null;
                    }
                });
            }
            var iRe = /<input([^>]*)>/g;
            while ((m = iRe.exec(box._html)) !== null) {
                var a = attrsOf(m[1]);
                box._fields.push({
                    tagName: 'INPUT',
                    type: a.type || 'text',
                    id: a.id || '',
                    value: a.value === undefined ? '' : a.value,
                    checked: false,
                    parentNode: box,
                    getAttribute: function (n) { return n === 'id' ? this.id : null; }
                });
            }
        }
    });
    return box;
}

function attrsOf(src) {
    var attrs = {};
    var re = /([\w:-]+)(?:="([^"]*)")?/g;
    var m;
    while ((m = re.exec(src)) !== null) {
        attrs[m[1]] = m[2] === undefined ? '' : m[2];
    }
    return attrs;
}

/**
 * makePanel — контейнер #ggufDetailPanel. В настоящем DOM форма лежит ВНУТРИ
 * панели (#ggufDetailContent), а не рядом: пересборка панели уничтожает и форму.
 * Поэтому панель делегирует поля/секции вложенному содержимому — так же, как
 * querySelectorAll панели находит узлы внутри #ggufDetailContent.
 */
function makePanel(paneBox) {
    var box = {
        id: 'ggufDetailPanel',
        tagName: 'DIV',
        parentNode: null,
        _html: '',
        querySelectorAll: function (selector) {
            return paneBox.querySelectorAll(selector);
        },
        querySelector: function () { return null; }
    };
    Object.defineProperty(box, 'innerHTML', {
        get: function () { return box._html; },
        set: function (html) {
            box._html = String(html);
            var m = /<div id="ggufDetailContent"[^>]*>([\s\S]*)<\/div>\s*$/.exec(box._html);
            paneBox.innerHTML = m ? m[1] : box._html;
        }
    });
    return box;
}

// Серверные значения формы: меняются между проходами, как настоящий ответ
// GET /api/v1/cppworker/config. «Открыта» только первая секция — как рисует
// collapseSections.
var server = { ctx: 65536, batch: 512 };
var renderCount = 0;

function renderHtml() {
    return '' +
        '<details class="gguf-acc" data-acc="1" open><summary>General</summary>' +
            '<input id="ggufOptCtxSize" type="number" value="' + server.ctx + '">' +
            '<input id="ggufOptBatchSize" type="number" value="' + server.batch + '">' +
        '</details>' +
        '<details class="gguf-acc" data-acc="2"><summary>Multi-GPU</summary></details>' +
        '<details class="gguf-acc" data-acc="3"><summary>KV cache</summary>' +
            '<input id="ggufOptKvCacheType" type="text" value="f16">' +
        '</details>';
}

// --- окружение ---------------------------------------------------------------

var pageEl = { id: 'gguf-page', tagName: 'DIV', parentNode: null };
var pane = makeContainer('ggufDetailContent', pageEl);
var panel = makePanel(pane);

global.window = global;
global.document = {
    activeElement: null,
    getElementById: function (id) {
        if (id === 'ggufDetailContent') return pane;
        if (id === 'ggufDetailPanel') return panel;
        if (id === 'gguf-page') return pageEl;
        return null;
    },
    addEventListener: function () {},
    removeEventListener: function () {},
    querySelectorAll: function () { return []; },
    visibilityState: 'visible'
};
global.GgufModule = {
    state: {
        selectedBackendId: 'b1',
        detailPane: 'settings',
        backends: [],
        localModels: [],
        loadedModels: [],
        runtimeModels: {}
    }
};
global.I18N = { t: function (k) { return k; } };

// Настоящие модули защиты и настоящий refresh-модуль (он определяет
// refreshDetailPanel/refreshDetailPane — их и проверяем).
require(path.join(root, 'js', 'modules', 'refresh-guard.js'));
require(path.join(root, 'js', 'modules', 'form-dirty.js'));
const SafeForm = require(path.join(root, 'js', 'modules', 'safe-form.js'));
const guardFile = path.join(root, 'js', 'modules', 'gguf-detail-form-state.js');
const formState = fs.existsSync(guardFile) ? require(guardFile) : null;
require(path.join(root, 'js', 'modules', 'gguf-renderer-refresh.js'));

const M = global.GgufModule;
M.currentBackend = function () { return { id: 'b1', url: 'http://x' }; };
M.renderDetailPane = function () { renderCount++; return renderHtml(); };
M.renderDetailPanel = function () {
    renderCount++;
    // Как renderDetailPanel в бою: заголовок + табы + содержимое в #ggufDetailContent.
    return '<div class="gguf-detail-header">b1</div>' +
        '<div id="ggufDetailContent" class="gguf-detail-content">' + renderHtml() + '</div>';
};

// --- каркас проверок ---------------------------------------------------------

var passed = 0;
var failures = [];
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

function field(id) {
    var list = pane._fields.filter(function (f) { return f.id === id; });
    assert.ok(list.length, 'в панели нет поля #' + id + ' — проверка потеряла точку отсчёта');
    return list[0];
}

function section(acc) {
    var list = pane._details.filter(function (d) { return d.getAttribute('data-acc') === String(acc); });
    assert.ok(list.length, 'в панели нет секции data-acc=' + acc);
    return list[0];
}

function openSections() {
    return pane._details.filter(function (d) { return d.open === true; })
        .map(function (d) { return d.getAttribute('data-acc'); });
}

// Начальное состояние: форма отрисована явным действием (клик по табу), как в
// жизни — первый рендер не защищён и объявляет базовые значения.
server = { ctx: 65536, batch: 512 };
renderCount = 0;
M.refreshDetailPane();
assert.strictEqual(renderCount, 1, 'предпосылка: явный рендер панели выполнился');
assert.strictEqual(field('ggufOptCtxSize').value, '65536', 'предпосылка: форма получила серверное значение');

// --- (1) два цикла авто-обновления не трогают заполняемую форму --------------
check('R688(а): набрал → ушёл из поля → 2 авто-прохода не пересобирают панель', function () {
    // Оператор раскрыл «KV cache» и ввёл n_ctx, затем кликнул мимо.
    section(3).open = true;
    field('ggufOptCtxSize').value = '9999';
    global.FormDirty.noteEdit(field('ggufOptCtxSize'));
    document.activeElement = null; // фокус уже не в поле — сценарий жалобы
    assert.strictEqual(global.FormDirty.isDirtyPage('gguf-page'), true, 'предпосылка: страница «грязная»');

    var before = renderCount;
    var deferred = 0;
    for (var i = 0; i < 2; i++) {
        if (M.refreshDetailPane({ auto: true }) === false) { deferred++; }
        M.refreshDetailPanel({ auto: true });
    }
    assert.strictEqual(deferred, 2,
        'авто-проход не откладывает пересборку панели, пока в форме есть правки (тик — 5 с)');
    assert.strictEqual(renderCount, before,
        'рендер вызывался во время заполнения формы — значит DOM пересобирался');
    assert.strictEqual(field('ggufOptCtxSize').value, '9999',
        'набранное значение стёрто авто-обновлением — живая жалоба «форма скрывается»');
    assert.deepStrictEqual(openSections(), ['1', '3'],
        'раскрытые секции схлопнулись во время авто-обновления: ' + JSON.stringify(openSections()));
});

// --- (2) ОДНА пересборка по другой причине не теряет ввод --------------------
check('R688(б): пересборка по другой причине сохраняет ввод и раскрытые секции', function () {
    server = { ctx: 32768, batch: 1024 }; // сервер успел изменить значения
    var before = renderCount;
    M.refreshDetailPane(); // явное действие: смена бэкенда / клик по табу
    assert.strictEqual(renderCount, before + 1, 'явное действие обязано перерисовать панель');
    assert.strictEqual(field('ggufOptCtxSize').value, '9999',
        'значение оператора потеряно при пересборке по другой причине (R688 п.4)');
    assert.strictEqual(field('ggufOptBatchSize').value, '1024',
        'соседнее поле не получило серверное значение — рендер не дошёл до новых данных');
    assert.deepStrictEqual(openSections(), ['1', '3'],
        'раскрытые секции схлопнулись при пересборке: ' + JSON.stringify(openSections()));
});

// --- (3) сохранение снимает защиту ------------------------------------------
check('R688(в): после сохранения авто-проход снова применяет серверные значения', function () {
    assert.ok(formState && typeof formState.noteSaved === 'function',
        'модуль защиты не экспортирует noteSaved — сохранение не снимет защиту штатно');
    formState.noteSaved(pane);
    assert.strictEqual(global.FormDirty.isDirtyPage('gguf-page'), false,
        'noteSaved не снял отметку правки страницы');
    var before = renderCount;
    var rebuilt = M.refreshDetailPane({ auto: true });
    assert.strictEqual(rebuilt, true, 'после сохранения авто-проход обязан пересобрать панель');
    assert.ok(renderCount > before, 'панель не перерисована: форма осталась со старыми значениями');
    assert.strictEqual(field('ggufOptCtxSize').value, '32768',
        'после сохранения форма не показывает актуальное серверное значение');
    assert.deepStrictEqual(openSections(), ['1', '3'],
        'раскрытые секции потерялись при обновлении после сохранения');
});

// --- (4) защита не превращается в «панель не обновляется никогда» ------------
check('R688(г): «грязная» страница без правок в панели не запрещает пересборку', function () {
    // Правка оператора — в другом месте страницы (страница «грязная»), в самой
    // панели полей он не менял: данные обязаны обновляться.
    global.FormDirty.noteEdit({
        tagName: 'INPUT', type: 'text', id: 'ggufHfDetailSearch', parentNode: pageEl
    });
    assert.strictEqual(global.FormDirty.isDirtyPage('gguf-page'), true, 'предпосылка: страница «грязная»');
    server = { ctx: 40960, batch: 2048 };
    var before = renderCount;
    var rebuilt = M.refreshDetailPane({ auto: true });
    assert.strictEqual(rebuilt, true, 'авто-проход отложен, хотя в панели нечего защищать');
    assert.ok(renderCount > before, 'панель не пересобралась — данные замерли');
    assert.strictEqual(field('ggufOptCtxSize').value, '40960',
        'панель не получила новые серверные значения');
});

// --- (5) статическая обвязка -------------------------------------------------
var appDataSrc = read('js/modules/app-data.js');
var providerSrc = read('js/modules/gguf-page-provider.js');
var refreshSrc = read('js/modules/gguf-renderer-refresh.js');
var settingsSrc = read('js/modules/gguf-detail-settings.js');
var paramsSrc = read('js/modules/gguf-detail-params.js');
var indexHtml = read('index.html');

check('R688(д): GGUF в SAFE_FORM_PAGES — данные страницы не глушатся', function () {
    assert.ok(/SAFE_FORM_PAGES\s*=\s*\{[^}]*\bgguf:\s*true/.test(appDataSrc),
        'GGUF не добавлен в SAFE_FORM_PAGES: «грязная» страница глушит тик целиком (0 запросов за 20 с)');
});

check('R688(е): провайдер страницы передаёт признак авто-прохода', function () {
    assert.ok(/M\.refreshDetail\(\s*\{\s*auto:\s*true\s*\}\s*\)/.test(providerSrc),
        'провайдер зовёт refreshDetail() без {auto:true} — авто-проход снова пересоберёт форму');
});

check('R688(ж): пересборка панели идёт через модуль защиты', function () {
    assert.ok(/GgufDetailFormState/.test(refreshSrc),
        'refreshDetailPanel/refreshDetailPane не спрашивают GgufDetailFormState — защита не подключена');
    assert.ok(/collectInputs/.test(read('js/modules/gguf-detail-form-state.js')),
        'модуль защиты не использует SafeForm.collectInputs — значения не переживут пересборку');
    assert.ok(/rememberOpen/.test(read('js/modules/gguf-detail-form-state.js')),
        'модуль защиты не помнит раскрытые секции');
});

check('R688(з): index.html подключает модуль защиты ДО refresh-модуля', function () {
    var guardIdx = indexHtml.indexOf('js/modules/gguf-detail-form-state.js');
    var refreshIdx = indexHtml.indexOf('js/modules/gguf-renderer-refresh.js');
    assert.ok(guardIdx > 0, 'index.html не подключает js/modules/gguf-detail-form-state.js');
    assert.ok(refreshIdx > guardIdx,
        'модуль защиты подключён ПОСЛЕ refresh-модуля: window.GgufDetailFormState будет undefined');
});

check('R688(и): сохранение настроек и профиля модели снимает защиту', function () {
    var saveStart = settingsSrc.indexOf('async function saveBackendOptions(');
    assert.ok(saveStart > 0, 'в gguf-detail-settings.js нет saveBackendOptions');
    var saveBody = settingsSrc.slice(saveStart, saveStart + 12000);
    assert.ok(/noteSaved\(/.test(saveBody),
        'saveBackendOptions не зовёт noteSaved: после сохранения страница остаётся «грязной» до TTL');
    var applyStart = paramsSrc.indexOf('async function applyModelParams(');
    assert.ok(applyStart > 0, 'в gguf-detail-params.js нет applyModelParams');
    var applyBody = paramsSrc.slice(applyStart, applyStart + 6000);
    assert.ok(/noteSaved\(/.test(applyBody),
        'applyModelParams не зовёт noteSaved: после применения профиля защита не снимается');
});

console.log('');
if (failures.length) {
    console.error('Провалов: ' + failures.length);
    failures.forEach(function (f) { console.error('  - ' + f); });
    process.exit(1);
}
console.log('Пройдено проверок: ' + passed);
