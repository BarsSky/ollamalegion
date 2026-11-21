// page-contract-r676.test.js — R676 (2026-11-21): КОНТРАКТ СТРАНИЦЫ В ФАБРИКАХ.
//
// ЖИВОЙ ДЕФЕКТ (доказан замером, не гипотеза). `app.js` передавал в фабрики ключ
// `currentPage` со значением-функцией (`currentPage: function () { return
// currentPage; }`), а `app-data.js:29` и `app-models.js:27` привязывали его к
// локальной переменной (`var currentPage = ctx.currentPage;`). Внутри фабрики
// `currentPage` была ФУНКЦИЕЙ, поэтому:
//
//   * `page === currentPage` (app-data.js) — ложь ВСЕГДА;
//   * `document.getElementById(currentPage + '-page')` — null (в id уходила
//     строка с исходником функции);
//   * `FormDirty.isDirtyPage(currentPage)` — false;
//   * `currentPage === 'backends'` / `=== 'logs'` (app-models.js) — ложь ВСЕГДА.
//
// Итог: тик авто-обновления не перерисовывал ни одной страницы, защиты
// R662/R666/R670 в этих файлах были мёртвым кодом, панель «Бэкенды» не
// обновлялась после исключения узла. Правильный образец — `app-logs.js:20`
// (`var getCurrentPage = ctx.getCurrentPage;` + вызовы) и `app-settings.js:26`.
//
// ЧТО ПРОВЕРЯЕТСЯ
//   1. В файлах фабрик не осталось ни привязки `ctx.currentPage`, ни сравнений
//      `currentPage === '<строка>'`, ни склейки `currentPage + '-page'`.
//   2. Каждая фабрика, читающая страницу, получает `getCurrentPage` в вызывающем
//      литерале и ВЫЗЫВАЕТ его; ключа `currentPage: function …` в app.js нет.
//   3. `index.html` подключает `safe-form.js` (?v=R676) после utils/form-dirty и
//      до всех модулей, которые пишут в поля.
//   4. Поведение SafeForm: три правила записи, force, база значения,
//      collect/restore для пересборок через innerHTML.
//   5. CI-чекер контракта фабрик (`scripts/webui_ctx_contract_check.js`) остаётся
//      зелёным: новое глобальное имя SafeForm не потребовало исключения.
//
// Запуск: node webui/tests/page-contract-r676.test.js
'use strict';

const assert = require('assert');
const { execFileSync } = require('child_process');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const root = path.join(__dirname, '..');            // webui/
const repoRoot = path.join(root, '..');             // корень репозитория
const read = (rel) => fs.readFileSync(path.join(root, rel), 'utf8');

const indexHtml = read('index.html');
const appJs = read('js/app.js');
const safeFormSrc = read('js/modules/safe-form.js');

// Фабрики, которые читают текущую страницу (образец — app-logs.js).
const PAGE_FACTORIES = ['app-data.js', 'app-models.js', 'app-settings.js', 'app-logs.js'];

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

/**
 * codeOf — исходник без комментариев.
 *
 * Нужно потому, что объяснения дефекта в шапках файлов ЦИТИРУЮТ дефектный код
 * (`var currentPage = ctx.currentPage;`, `currentPage === '<строка>'`). Проверка
 * по сырому тексту краснела бы на комментарии, объясняющем, почему так писать
 * нельзя, — то есть наказывала бы за документацию.
 */
function codeOf(src) {
    const noBlock = src.replace(/\/\*[\s\S]*?\*\//g, '');
    return noBlock.split('\n').map(function (line) {
        const i = line.indexOf('//');
        return i === -1 ? line : line.slice(0, i);
    }).join('\n');
}

// --- (1) в фабриках нет старого контракта -----------------------------------
check('в фабриках нет привязки ctx.currentPage (страница приходит геттером)', () => {
    PAGE_FACTORIES.forEach(function (name) {
        const code = codeOf(read('js/modules/' + name));
        assert.ok(!/ctx\s*\.\s*currentPage/.test(code),
            name + ': фабрика всё ещё читает ctx.currentPage — это снова снимок,' +
            ' а вызывающий передаёт функцию (дефект R676)');
    });
});

check('в фабриках нет сравнений/склейки по currentPage', () => {
    PAGE_FACTORIES.forEach(function (name) {
        const code = codeOf(read('js/modules/' + name));
        assert.ok(!/\bcurrentPage\s*[!=]==?\s*['"]/.test(code),
            name + ': сравнение currentPage со строкой — в фабрике это имя функции,' +
            ' условие ложно всегда (ровно корень R676)');
        assert.ok(!/\bcurrentPage\s*\+\s*['"]/.test(code),
            name + ': склейка currentPage + "-page" — в id уйдёт исходник функции, getElementById вернёт null');
        assert.ok(!/\bvar\s+currentPage\b/.test(code),
            name + ': в фабрике объявлена локальная currentPage — имя снова означает не то, что кажется');
    });
});

// --- (2) каждая фабрика получает геттер и вызывает его -----------------------
check('каждая фабрика страницы привязывает и ВЫЗЫВАЕТ getCurrentPage', () => {
    PAGE_FACTORIES.forEach(function (name) {
        const code = codeOf(read('js/modules/' + name));
        assert.ok(/var\s+getCurrentPage\s*=\s*ctx\.getCurrentPage\s*;/.test(code),
            name + ': нет привязки `var getCurrentPage = ctx.getCurrentPage;` — контракт страницы потерян');
        assert.ok(/getCurrentPage\(\s*\)/.test(code),
            name + ': привязка есть, но страница ни разу не читается вызовом — тик не узнает, что открыто');
    });
});

check('вызывающие литералы передают getCurrentPage, а не значение', () => {
    const code = codeOf(appJs);
    const getters = (code.match(/getCurrentPage:\s*function\s*\(\s*\)\s*\{\s*return\s+currentPage;\s*\}/g) || []);
    assert.ok(getters.length >= PAGE_FACTORIES.length,
        'в app.js меньше ' + PAGE_FACTORIES.length + ' литералов с getCurrentPage (найдено ' +
        getters.length + ') — фабрика получит undefined, и страница снова станет «вечной dashboard»');
    assert.ok(!/^\s*currentPage\s*:\s*function/m.test(code),
        'в app.js вернулся литерал `currentPage: function () …` — это и был дефект R676 ' +
        '(имя-значение содержит функцию, фабрика сравнивает его со строкой)');
    assert.ok(!/^\s*currentPage\s*:/m.test(code),
        'в app.js появился ключ `currentPage:` в литерале фабрики — страница обязана ' +
        'передаваться только геттером getCurrentPage');
});

// --- (3) safe-form.js подключён в правильном порядке ------------------------
check('index.html подключает safe-form.js (?v=R676) до модулей, которые пишут в поля', () => {
    assert.ok(/js\/modules\/safe-form\.js\?v=R676/.test(indexHtml),
        'index.html не подключает js/modules/safe-form.js?v=R676 — window.SafeForm будет undefined, ' +
        'и защита полей выключена целиком');
    const at = (needle) => {
        const i = indexHtml.indexOf(needle);
        assert.ok(i > 0, 'index.html не подключает ' + needle + ' — проверка потеряла точку отсчёта');
        return i;
    };
    const safe = at('js/modules/safe-form.js');
    // После utils.js (общие помощники) и form-dirty.js (страница-владелец правки).
    assert.ok(safe > at('js/modules/utils.js'), 'safe-form.js обязан грузиться ПОСЛЕ utils.js');
    assert.ok(safe > at('js/modules/form-dirty.js'),
        'safe-form.js читает window.FormDirty — подключать его раньше form-dirty.js нельзя');
    // До всех модулей, которые пишут в поля.
    ['js/modules/settings-ui.js', 'js/modules/settings-registry.js', 'js/modules/config-io.js',
        'js/modules/autotune_settings.js', 'js/modules/app-settings.js', 'js/modules/app-models.js',
        'js/modules/app-data.js', 'js/app.js'].forEach(function (rel) {
        assert.ok(safe < at(rel), 'safe-form.js обязан грузиться ДО ' + rel + ' (иначе запись в поля идёт мимо него)');
    });
});

// --- (4) поведение SafeForm -------------------------------------------------
function buildSafeForm(opts) {
    const options = opts || {};
    const sandboxWindow = options.window || {};
    const sandbox = {
        window: sandboxWindow,
        document: {
            activeElement: options.activeElement || null,
            getElementById() { return null; }
        },
        module: { exports: {} }
    };
    sandbox.window = sandboxWindow;
    vm.createContext(sandbox);
    vm.runInContext(safeFormSrc, sandbox, { filename: 'safe-form.js' });
    assert.ok(sandboxWindow.SafeForm, 'safe-form.js не выставил window.SafeForm');
    return { api: sandboxWindow.SafeForm, sandbox: sandbox, window: sandboxWindow };
}

function fieldEl(props) {
    const page = props && props.page ? props.page : null;
    return Object.assign({
        id: 'ctxSize',
        tagName: 'INPUT',
        type: 'number',
        value: '',
        checked: false,
        parentNode: page
    }, (props && props.el) || {});
}

check('SafeForm: чистое поле пишется, база запоминается', () => {
    const { api } = buildSafeForm();
    const el = fieldEl();
    assert.strictEqual(api.setValue(el, '2048'), true, 'запись в чистое поле обязана проходить');
    assert.strictEqual(el.value, '2048');
    assert.strictEqual(api.baseOf(el), '2048', 'база — то, что записал сам SafeForm');
});

check('SafeForm: поле в фокусе не трогается (правило 1)', () => {
    const el = fieldEl();
    const { api } = buildSafeForm({ activeElement: el });
    assert.strictEqual(api.setValue(el, '2048'), false, 'запись в поле с курсором запрещена');
    assert.strictEqual(el.value, '', 'значение в поле с фокусом изменено');
    assert.strictEqual(api.blockedReason(el), 'active', 'причина отказа обязана называться «active»');
});

check('SafeForm: поле, которое менял оператор, не трогается (правило 2)', () => {
    const { api } = buildSafeForm();
    const el = fieldEl();
    api.setValue(el, '2048');
    el.value = '9999'; // набор оператора
    assert.strictEqual(api.setValue(el, '2048'), false, 'набранное значение перетёрто серверным');
    assert.strictEqual(el.value, '9999', 'ввод оператора потерян');
    assert.strictEqual(api.blockedReason(el), 'edited');
    // Вернул базовое — снова управляется.
    el.value = '2048';
    assert.strictEqual(api.setValue(el, '4096'), true, 'после возврата к базовому поле должно обновляться');
    assert.strictEqual(el.value, '4096');
});

check('SafeForm: страница с несохранённой правкой защищает СВОИ поля (правило 3)', () => {
    const page = { id: 'settings-page', tagName: 'DIV', parentNode: null };
    const el = fieldEl({ page: page });
    const win = { FormDirty: { isDirtyPage: function (id) { return id === 'settings-page'; } } };
    const { api } = buildSafeForm({ window: win });
    api.setValue(el, '2048'); // база есть, правки в этом поле нет
    el.value = '2048';
    assert.strictEqual(api.setValue(el, '4096'), false, 'на грязной странице поле перетёрто');
    assert.strictEqual(el.value, '2048');
    assert.strictEqual(api.blockedReason(el), 'dirty');
    // force — запись-ответ на действие оператора (пресет, импорт конфига).
    assert.strictEqual(api.setValue(el, '4096', { force: true }), true, 'force обязан обходить защиту');
    assert.strictEqual(el.value, '4096');
    assert.strictEqual(api.baseOf(el), '4096', 'force-запись тоже обязана запомнить базу');
});

check('SafeForm: setChecked уважает те же правила, stats считает отказы', () => {
    const el = fieldEl({ el: { type: 'checkbox', checked: false } });
    const { api } = buildSafeForm({ activeElement: el });
    api.resetStats();
    assert.strictEqual(api.setChecked(el, true), false, 'чекбокс в фокусе не трогается');
    assert.strictEqual(api.stats().blocked, 1, 'отказ не посчитан — «защита работает» неотличимо от «не вызвали»');
    assert.strictEqual(el.checked, false);
});

check('SafeForm: markBase объявляет базу для разметки, нарисованной атрибутом value', () => {
    const { api } = buildSafeForm();
    const el = fieldEl({ el: { value: '4096' } }); // нарисовано серверным значением
    assert.strictEqual(api.baseOf(el), undefined, 'до markBase база неизвестна');
    api.markBase(el); // база = то, что уже нарисовано
    assert.strictEqual(api.baseOf(el), '4096');
    assert.strictEqual(api.setValue(el, '2048'), true, 'после markBase поле снова управляется');

    // Явная база: в поле ввод оператора (777), базой объявлено серверное 512.
    const el2 = fieldEl({ el: { id: 'batchSize', value: '777' } });
    api.markBase(el2, '512');
    assert.strictEqual(api.baseOf(el2), '512');
    assert.strictEqual(api.setValue(el2, '256'), false, 'поле, чьё значение расходится с базой, не трогается');
    assert.strictEqual(el2.value, '777', 'ввод оператора потерян');
});

// Поле панели реестра/таблицы лимитов: значение + ключ в data-атрибуте.
function keyedInput(key, value) {
    return {
        id: '', tagName: 'INPUT', type: 'number', value: value, checked: false,
        parentNode: { id: 'settings-page', tagName: 'DIV', parentNode: null },
        getAttribute(n) { return n === 'data-hot-input' ? key : null; }
    };
}

check('SafeForm: collect/restore сохраняют ввод оператора при пересборке innerHTML', () => {
    const { api } = buildSafeForm();
    const oldEl = keyedInput('LB_ADMISSION_WAIT_SEC', '10');
    api.setValue(oldEl, '10');      // база 10 (серверное значение)
    oldEl.value = '30';             // оператор набрал 30
    const oldRoot = { querySelectorAll() { return [oldEl]; } };
    const pending = api.collectInputs(oldRoot, 'data-hot-input');
    assert.deepStrictEqual(Object.keys(pending), ['LB_ADMISSION_WAIT_SEC'], 'правка оператора не собрана');
    assert.strictEqual(pending.LB_ADMISSION_WAIT_SEC, '30');

    // Панель перерисована: новый узел нарисован серверным значением 10.
    const newEl = keyedInput('LB_ADMISSION_WAIT_SEC', '10');
    const newRoot = { querySelectorAll() { return [newEl]; } };
    assert.strictEqual(api.restoreInputs(newRoot, pending, 'data-hot-input'), 1);
    assert.strictEqual(newEl.value, '30', 'ввод оператора не вернулся в новую разметку');
    assert.strictEqual(api.baseOf(newEl), '10', 'базой нового узла обязано остаться СЕРВЕРНОЕ значение');
    assert.strictEqual(api.setValue(newEl, '10'), false, 'после пересборки ввод оператора снова затирается');
});

check('SafeForm: после пересборки у нетронутых полей тоже появляется база', () => {
    // Таблица лимитов и панель реестра рисуются через innerHTML — их поля никогда
    // не проходят через setValue. Без adopt в restoreInputs коллектор не увидел бы
    // правку оператора в них, и следующая пересборка стёрла бы её молча.
    const { api } = buildSafeForm();
    const a = keyedInput('A', '10');
    const b = keyedInput('B', '20');
    const root = { querySelectorAll() { return [a, b]; } };
    api.restoreInputs(root, Object.create(null), 'data-hot-input');
    assert.strictEqual(api.baseOf(a), '10', 'нетронутое поле не получило базу');
    assert.strictEqual(api.baseOf(b), '20', 'нетронутое поле не получило базу');

    b.value = '99'; // оператор правит второе поле
    assert.deepStrictEqual(Object.keys(api.collectInputs(root, 'data-hot-input')), ['B'],
        'правка оператора в поле, которое рисовалось через innerHTML, не обнаружена');
});

// --- (5) CI-чекер контракта фабрик остаётся зелёным -------------------------
check('scripts/webui_ctx_contract_check.js зелёный (SafeForm не потребовал исключения)', () => {
    const checker = path.join(repoRoot, 'scripts', 'webui_ctx_contract_check.js');
    assert.ok(fs.existsSync(checker), 'нет scripts/webui_ctx_contract_check.js');
    let out = '';
    let status = 0;
    try {
        out = execFileSync(process.execPath, [checker], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
    } catch (e) {
        status = typeof e.status === 'number' ? e.status : 1;
        out = ((e.stdout || '') + (e.stderr || '')).toString();
    }
    assert.strictEqual(status, 0, 'чекер контракта фабрик красный:\n' + out);
    assert.ok(/фабрик window\.X = function \(ctx\) проверено: \d+/.test(out),
        'чекер не нашёл фабрик (проверка прошла «по пустоте»):\n' + out);
    const checkerSrc = fs.readFileSync(checker, 'utf8');
    assert.ok(!/name:\s*'SafeForm'/.test(checkerSrc),
        'SafeForm попал в KNOWN_FRAGILE: значит имя потерялось или читается, но не передаётся — ' +
        'исключение здесь маскировало бы дефект класса R671b');
    const fragileEntries = (checkerSrc.split('const KNOWN_FRAGILE = [')[1] || '').split('\n];')[0]
        .match(/\n    \{/g) || [];
    assert.strictEqual(fragileEntries.length, 4,
        'исключений в чекере стало ' + fragileEntries.length + ': список должен расти только с разбором');
});

if (failures.length) {
    console.error('\n' + failures.length + ' проверок провалено, ' + passed + ' пройдено');
    process.exit(1);
}
console.log('\nвсе ' + passed + ' проверок пройдены');
