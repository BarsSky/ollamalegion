// gguf-auto-refresh-stability-r689.test.js — R689 (2026-11-23): два дефекта
// правой панели GGUF, оставшихся после R688.
//
// ЖИВАЯ ЖАЛОБА (дословно): «Пока не вводится значение, интерфейс продолжает
// скакать при обновлении, но форму не закрывает. Когда же есть фокус на поле для
// ввода, то обновления скачками пропадают, и не понятно, обновляется ли статус».
//
// ЗАМЕР НА СТЕНДЕ ДО ПРАВКИ (CDP, образ r688, вкладка «Настройки» бэкенда,
// 20 с без ввода): 12 подмен DOM правой панели (8 панели + 4 содержимого) при
// НЕИЗМЕННЫХ данных; асинхронные блоки формы (cpp-load-defaults / cpp-model-catalog)
// после первой же подмены остались заглушкой «Загрузка…». С фокусом в поле:
// 0 запросов GET /api/v1/backends/<id> за 20 с, индикатор свежести в шапке старел
// до 21 с, строки статуса рядом с панелью не было вовсе.
//
// ЧТО ПРОВЕРЯЕТСЯ (поведенчески, на настоящих модулях):
//   1. НЕИЗМЕННЫЕ ДАННЫЕ → DOM НЕ ПОДМЕНЯЕТСЯ: четыре авто-прохода подряд при
//      одинаковом ответе дают НОЛЬ записей innerHTML и НЕ МЕНЯЮТ scrollTop.
//   1б. ПРЯМАЯ ДОРИСОВКА (гидратация формы/профилей) не провоцирует пересборку:
//      иначе следующий проход уничтожает только что приехавшие блоки.
//   2. ИЗМЕНИВШИЕСЯ ДАННЫЕ → подмена есть, но scrollTop восстановлен, раскрытые
//      секции сохранены, значение оператора перенесено.
//   3. ПОКА ПЕРЕРИСОВКА ОТЛОЖЕНА (фокус/правка в форме) рядом с панелью видна
//      строка «данные обновлены N с назад · перерисовка приостановлена, пока вы
//      вводите», и счётчик успешных обновлений РАСТЁТ на каждом проходе
//      провайдера страницы;
//   4. после сохранения (noteSaved) строка гаснет, серверные значения
//      применяются, защита не превращается в «панель не обновляется никогда»;
//   5. статическая обвязка: модуль строки подключён в index.html ДО модуля
//      защиты, разметка страницы содержит #ggufAutoStatus, провайдер сообщает об
//      успешном проходе, у тика есть список страниц, где данные не глушатся по
//      фокусу, ключи i18n есть в ru.js и en.js.
//
// ТЕСТ НЕ-ВАКУУМНЫЙ. Он запускается на настоящих модулях из каталога
// WEBUI_SRC_ROOT (по умолчанию — текущий webui/) и на коде до R689 КРАСНЕЕТ:
//
//   git archive 326d57e8 webui | tar -x -C <tmp>
//   WEBUI_SRC_ROOT=<tmp>/webui node webui/tests/gguf-auto-refresh-stability-r689.test.js
//
// Прогон R689 (доказательство в отчёте): 0/6 проверок на старом коде, 6/6 на новом.
//
// Запуск: node webui/tests/gguf-auto-refresh-stability-r689.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const SRC = process.env.WEBUI_SRC_ROOT
    ? path.resolve(process.env.WEBUI_SRC_ROOT)
    : path.join(__dirname, '..');
const read = (rel) => fs.readFileSync(path.join(SRC, rel), 'utf8');

// --- фейковый DOM ------------------------------------------------------------
//
// Контейнер, чей innerHTML пересоздаёт поля и секции — ровно так, как это делает
// браузер. Дополнительно имитируем АВАРИЮ ПРОКРУТКИ: присвоение innerHTML на
// мгновение убирает содержимое, и браузер роняет scrollTop прокручиваемого
// предка в его новый предел. В тесте «предел» — это 0, то есть потеря позиции.
// R689 обязан восстановить позицию после подмены (и не допускать подмены вовсе,
// когда данные не изменились).

function attrsOf(src) {
    var attrs = {};
    var re = /([\w:-]+)(?:="([^"]*)")?/g;
    var m;
    while ((m = re.exec(src)) !== null) {
        attrs[m[1]] = m[2] === undefined ? '' : m[2];
    }
    return attrs;
}

function makeContainer(id, pageEl) {
    var box = {
        id: id,
        tagName: 'DIV',
        parentNode: pageEl,
        _details: [],
        _fields: [],
        _html: '',
        writes: 0,
        querySelectorAll: function (selector) {
            var sel = String(selector || '').trim();
            if (sel === 'details') return box._details.slice();
            if (sel === 'input[id], select[id], textarea[id]') return box._fields.slice();
            return [];
        },
        querySelector: function () { return null; }
    };
    Object.defineProperty(box, 'innerHTML', {
        get: function () { return box._html; },
        set: function (html) {
            box.writes++;
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
            // «Прокрутка уехала»: содержимое пересоздано, высота упала.
            if (pageEl) pageEl.scrollTop = 0;
        }
    });
    return box;
}

/**
 * makePanel — контейнер #ggufDetailPanel. В настоящем DOM форма лежит ВНУТРИ
 * панели (#ggufDetailContent), а не рядом: пересборка панели уничтожает и форму.
 */
function makePanel(paneBox, pageEl) {
    var box = {
        id: 'ggufDetailPanel',
        tagName: 'DIV',
        parentNode: pageEl,
        _html: '',
        writes: 0,
        querySelectorAll: function (selector) {
            return paneBox.querySelectorAll(selector);
        },
        querySelector: function () { return null; }
    };
    Object.defineProperty(box, 'innerHTML', {
        get: function () { return box._html; },
        set: function (html) {
            box.writes++;
            box._html = String(html);
            var m = /<div id="ggufDetailContent"[^>]*>([\s\S]*)<\/div>\s*$/.exec(box._html);
            paneBox.innerHTML = m ? m[1] : box._html;
            if (pageEl) pageEl.scrollTop = 0;
        }
    });
    return box;
}

// --- окружение ---------------------------------------------------------------

// Прокручиваемый предок панели: в бою это контейнер страницы/документ.
var pageEl = { id: 'gguf-page', tagName: 'DIV', parentNode: null, scrollTop: 0, scrollLeft: 0 };
var pane = makeContainer('ggufDetailContent', pageEl);
var panel = makePanel(pane, pageEl);

// Строка статуса (R689): #ggufAutoStatus живёт ВНЕ панели — переживает пересборку.
var statusNode = {
    id: 'ggufAutoStatus',
    hidden: true,
    textContent: '',
    classList: { _c: [], add: function (c) { this._c.push(c); }, remove: function (c) { this._c = this._c.filter(function (x) { return x !== c; }); }, contains: function (c) { return this._c.indexOf(c) !== -1; } }
};
var statusTextNode = {
    id: 'ggufAutoStatusText',
    textContent: '',
    classList: { add: function () {}, remove: function () {}, contains: function () { return false; } }
};

global.window = global;
global.document = {
    activeElement: null,
    getElementById: function (id) {
        if (id === 'ggufDetailContent') return pane;
        if (id === 'ggufDetailPanel') return panel;
        if (id === 'gguf-page') return pageEl;
        if (id === 'ggufAutoStatus') return statusNode;
        if (id === 'ggufAutoStatusText') return statusTextNode;
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
global.I18N = { t: function (k, vars) {
    if (vars && vars.n !== undefined) return k + ' [' + vars.n + ']';
    if (vars && vars.age !== undefined) return k + ' [' + vars.age + ']';
    return k;
} };

// Настоящие модули защиты и настоящий refresh-модуль (он определяет
// refreshDetailPanel/refreshDetailPane — их и проверяем).
require(path.join(SRC, 'js', 'modules', 'refresh-guard.js'));
require(path.join(SRC, 'js', 'modules', 'form-dirty.js'));
require(path.join(SRC, 'js', 'modules', 'safe-form.js'));
var statusFile = path.join(SRC, 'js', 'modules', 'gguf-refresh-status.js');
var hasStatusModule = fs.existsSync(statusFile);
if (hasStatusModule) require(statusFile);
var guardFile = path.join(SRC, 'js', 'modules', 'gguf-detail-form-state.js');
require(guardFile);
require(path.join(SRC, 'js', 'modules', 'gguf-renderer-refresh.js'));
require(path.join(SRC, 'js', 'modules', 'gguf-page-provider.js'));

const M = global.GgufModule;
const formState = require(guardFile);

// Серверные значения формы: меняются между проходами, как настоящий ответ
// GET /api/v1/cppworker/config. «Открыта» только первая секция — как рисует
// collapseSections.
var server = { ctx: 65536, batch: 512 };

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

M.renderDetailPane = function () { return renderHtml(); };
M.renderDetailPanel = function () {
    return '<div class="gguf-detail-header">b1</div>' +
        '<div id="ggufDetailContent" class="gguf-detail-content">' + renderHtml() + '</div>';
};

// --- каркас проверок ---------------------------------------------------------

var passed = 0;
var failures = [];
var checks = [];
function check(name, fn) { checks.push([name, fn]); }

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
function statusLabel() { return String(statusTextNode.textContent || ''); }
function resetStatusLine() {
    statusNode.hidden = true;
    statusNode.textContent = '';
    statusTextNode.textContent = '';
}

// Начальное состояние: форма отрисована явными действиями (клик по табу + тик),
// как в жизни — первый рендер не защищён и объявляет базовые значения.
server = { ctx: 65536, batch: 512 };
global.FormDirty.clearAll();
document.activeElement = null;
M.refreshDetailPane();   // содержимое панели
M.refreshDetailPanel();  // панель целиком (заголовок + табы + содержимое)
assert.strictEqual(field('ggufOptCtxSize').value, '65536', 'предпосылка: форма получила серверное значение');

// --- (1) неизменные данные → DOM не подменяется, scrollTop не меняется -------

check('R689(а): 4 авто-прохода при неизменных данных — 0 подмен DOM и scrollTop цел', function () {
    // Оператор посмотрел форму и отпрокрутил страницу; данные на сервере не
    // меняются (тик просто подтверждает то же состояние).
    pageEl.scrollTop = 547;
    var writesBefore = pane.writes + panel.writes;
    var rebuilt = 0;
    for (var i = 0; i < 4; i++) {
        if (M.refreshDetailPane({ auto: true }) === true) rebuilt++;
        if (M.refreshDetailPanel({ auto: true }) === true) rebuilt++;
    }
    assert.strictEqual(pane.writes + panel.writes, writesBefore,
        'DOM панели подменён ' + ((pane.writes + panel.writes) - writesBefore) +
        ' раз при НЕИЗМЕННЫХ данных — это и есть «интерфейс продолжает скакать»');
    assert.strictEqual(rebuilt, 0, 'пересборка не должна даже считаться выполненной');
    assert.strictEqual(pageEl.scrollTop, 547,
        'scrollTop уехал: ' + pageEl.scrollTop + ' вместо 547 — «прыжок прокрутки»');
    assert.strictEqual(field('ggufOptCtxSize').value, '65536', 'значение поля испорчено');
});

// --- (2) прямая дорисовка не провоцирует пересборку --------------------------

check('R689(ж): дорисованные напрямую блоки не уничтожаются авто-проходом', function () {
    assert.strictEqual(typeof formState.rebaseline, 'function',
        'нет rebaseline — гидратация формы вызовет лишнюю пересборку панели');
    // Ровно как после loadAndRenderBackendOptions: форма нарисована напрямую,
    // снимок объявлен соответствующим текущему рендеру.
    formState.rebaseline(pane, M.renderDetailPane());
    formState.rebaseline(panel, M.renderDetailPanel());
    // Асинхронные блоки (профили модели / defaults / catalog) дорисовались в DOM.
    pane._html += '<div class="cpp-profiles-empty">profiles</div>';
    var writesBefore = pane.writes + panel.writes;
    for (var i = 0; i < 2; i++) {
        M.refreshDetailPane({ auto: true });
        M.refreshDetailPanel({ auto: true });
    }
    assert.strictEqual(pane.writes + panel.writes, writesBefore,
        'дорисованные блоки уничтожены пересборкой (снова заглушка «Загрузка…»)');
    assert.ok(/profiles/.test(pane._html), 'дорисованный блок пропал из DOM');
});

// --- (3) изменившиеся данные → подмена есть, но состояние сохранено ----------

check('R689(б): при новых данных подмена есть, scrollTop/секции/значение сохранены', function () {
    // Оператор раскрыл «KV cache» и ввёл n_ctx; страница прокручена.
    section(3).open = true;
    field('ggufOptCtxSize').value = '9999';
    global.FormDirty.noteEdit(field('ggufOptCtxSize'));
    pageEl.scrollTop = 547;
    // Сервер прислал новые значения — панель обязана их показать.
    server = { ctx: 32768, batch: 1024 };
    var writesBefore = pane.writes;
    var rebuilt = M.refreshDetailPane(); // явная пересборка (смена данных)
    assert.strictEqual(rebuilt, true, 'изменившиеся данные обязаны перерисовать панель');
    assert.ok(pane.writes > writesBefore, 'DOM не подменён, хотя данные изменились');
    assert.strictEqual(pageEl.scrollTop, 547,
        'scrollTop не восстановлен после подмены: ' + pageEl.scrollTop + ' (было 547)');
    assert.strictEqual(field('ggufOptCtxSize').value, '9999', 'значение оператора потеряно при подмене');
    assert.strictEqual(field('ggufOptBatchSize').value, '1024', 'соседнее поле не получило серверное значение');
    assert.deepStrictEqual(openSections(), ['1', '3'],
        'раскрытые секции схлопнулись: ' + JSON.stringify(openSections()));
});

// --- (3) при защищённом вводе видно, что данные идут и почему панель стоит ---

check('R689(в): перерисовка отложена, но видна строка «данные обновлены … · приостановлена»', function () {
    assert.ok(global.GgufRefreshStatus,
        'нет window.GgufRefreshStatus: оператор не видит ни свежести, ни причины паузы');
    resetStatusLine();
    // Оператор в поле: и фокус, и правка — защита активна.
    document.activeElement = field('ggufOptCtxSize');
    field('ggufOptCtxSize').value = '1234';
    global.FormDirty.noteEdit(field('ggufOptCtxSize'));
    var writesBefore = pane.writes + panel.writes;
    // Данные уже обновлялись 12 с назад — ровно такую подпись ждём.
    global.GgufRefreshStatus.noteSuccess(Date.now() - 12000);
    assert.strictEqual(M.refreshDetailPane({ auto: true }), false, 'пересборка при активной форме обязана откладываться');
    assert.strictEqual(pane.writes + panel.writes, writesBefore, 'отложенная пересборка всё-таки подменила DOM');
    assert.strictEqual(statusNode.hidden, false, 'строка статуса скрыта — оператор не знает, что происходит');
    var text = statusLabel();
    assert.ok(/12/.test(text), 'в строке нет возраста последнего обновления: ' + JSON.stringify(text));
    assert.ok(/приостановлена/.test(text) || /paused/i.test(text),
        'в строке нет причины («перерисовка приостановлена»): ' + JSON.stringify(text));
    assert.strictEqual(field('ggufOptCtxSize').value, '1234', 'значение в поле не должно меняться от прохода');
});

// --- (4) счётчик успешных обновлений растёт на каждом проходе провайдера -----

check('R689(г): провайдер страницы считает успешные обновления, пока панель стоит', function () {
    assert.ok(global.GgufRefreshStatus, 'нет строки статуса');
    // Настоящий провайдер страницы: регистрируем его в двойнике DataRefresh
    // (при загрузке модуля реестра ещё не было, поэтому флаг registered снят).
    var providerFn = null;
    global.DataRefresh = {
        register: function (page, fn) { providerFn = fn; return true; }
    };
    assert.strictEqual(global.GgufPageProvider.register(), true, 'провайдер не зарегистрировался');
    assert.strictEqual(typeof providerFn, 'function', 'DataRefresh не получил провайдера страницы');

    // refreshDetail в этом окружении заменяем на реальный refreshDetailPane:
    // ровно это делает refreshDetail в конце своего прохода (после сети).
    M.refreshDetail = function () { M.refreshDetailPane({ auto: true }); return Promise.resolve(); };
    M.refreshBackends = function () { return Promise.resolve(); };
    M.refreshActiveDownloads = function () { return Promise.resolve(); };

    var before = statusLabel();
    var seq = [];
    function runProviderOnce() {
        return Promise.resolve(providerFn()).then(function () { seq.push(statusLabel()); });
    }
    return runProviderOnce().then(runProviderOnce).then(runProviderOnce).then(function () {
        var last = statusLabel();
        assert.ok(/обновлен|updates/i.test(last), 'в строке нет счётчика обновлений: ' + JSON.stringify(last));
        var numbers = seq.map(function (t) {
            var m = String(t).match(/(\d+)\D*$/);
            return m ? Number(m[1]) : -1;
        });
        assert.deepStrictEqual(numbers, [1, 2, 3],
            'счётчик успешных обновлений не растёт: ' + JSON.stringify(seq));
        assert.ok(before !== last, 'строка статуса не изменилась после трёх обновлений');
        assert.strictEqual(statusNode.hidden, false, 'строка статуса исчезла, пока защита активна');
    });
});

// --- (5) сохранение возвращает управление панели -----------------------------

check('R689(д): после сохранения строка гаснет, серверные значения применяются', function () {
    // Оператор сохранил: защита снимается, значения объявляются базовыми.
    assert.strictEqual(global.FormDirty.isDirtyPage('gguf-page'), true, 'предпосылка: страница «грязная»');
    formState.noteSaved(pane);
    document.activeElement = null;
    server = { ctx: 40960, batch: 2048 };
    var writesBefore = pane.writes;
    var rebuilt = M.refreshDetailPane({ auto: true });
    assert.strictEqual(rebuilt, true, 'после сохранения панель обязана обновиться серверными значениями');
    assert.ok(pane.writes > writesBefore, 'панель не перерисована после сохранения');
    assert.strictEqual(field('ggufOptCtxSize').value, '40960',
        'после сохранения форма не показывает актуальное серверное значение');
    assert.strictEqual(statusNode.hidden, true,
        'строка «перерисовка приостановлена» осталась на экране, хотя панель снова обновляется');
});

// --- (6) статическая обвязка -------------------------------------------------

check('R689(е): разметка, порядок скриптов, провайдер, тик и i18n на месте', function () {
    var indexHtml = read('index.html');
    var rendererSrc = read('js/modules/gguf-renderer.js');
    var providerSrc = read('js/modules/gguf-page-provider.js');
    var appDataSrc = read('js/modules/app-data.js');
    var ruSrc = read('js/i18n/ru.js');
    var enSrc = read('js/i18n/en.js');

    assert.ok(/id="ggufAutoStatus"/.test(rendererSrc),
        'в разметке страницы GGUF нет строки статуса #ggufAutoStatus');
    assert.ok(/class="gguf-auto-status"/.test(rendererSrc),
        'строка статуса не оформлена классом .gguf-auto-status');
    var statusIdx = indexHtml.indexOf('js/modules/gguf-refresh-status.js');
    var guardIdx = indexHtml.indexOf('js/modules/gguf-detail-form-state.js');
    assert.ok(statusIdx > 0, 'index.html не подключает js/modules/gguf-refresh-status.js');
    assert.ok(guardIdx > statusIdx,
        'модуль защиты подключён ДО строки статуса: отложенная пересборка останется без объяснения');
    assert.ok(/GgufRefreshStatus/.test(providerSrc) && /noteSuccess/.test(providerSrc),
        'провайдер страницы не сообщает об успешном обновлении — счётчик не растёт');
    assert.ok(/REFRESH_WHILE_TYPING_PAGES\s*=\s*\{[^}]*\bgguf:\s*true/.test(appDataSrc),
        'тик авто-обновления по-прежнему глушит страницу GGUF целиком, пока фокус в поле');
    ['gguf.auto.data_just_now', 'gguf.auto.data_ago', 'gguf.auto.redraw_paused', 'gguf.auto.updates_count']
        .forEach(function (key) {
            assert.ok(new RegExp('"' + key.replace(/\./g, '\\.') + '"\\s*:').test(ruSrc),
                'нет ключа ' + key + ' в ru.js');
            assert.ok(new RegExp('"' + key.replace(/\./g, '\\.') + '"\\s*:').test(enSrc),
                'нет ключа ' + key + ' в en.js');
        });
});

// --- прогон ------------------------------------------------------------------

(async function main() {
    for (var i = 0; i < checks.length; i++) {
        var name = checks[i][0];
        try {
            await checks[i][1]();
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
