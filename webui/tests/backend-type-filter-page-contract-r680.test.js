// backend-type-filter-page-contract-r680.test.js — R680 (2026-11-21): контракт
// страницы у модуля-СИНГЛТОНА (BackendTypeFilter).
//
// ЖИВОЙ ДЕФЕКТ (закрыт здесь). toggleGgufTab/toggleImageTab уходили с исчезнувшей
// страницы так:
//
//     if (!show && window.ui && window.ui.currentPage === 'gguf') {
//         if (typeof window.ui.switchPage === 'function') window.ui.switchPage('dashboard');
//     }
//
// Но объект `ui` (const в app.js, публикуется как window.ui) НИ `currentPage`, НИ
// `switchPage` не публикует: `switchPage` объявлен внутри IIFE и в возвращаемый
// объект не входит. То есть обе ветки «страницу спрятали, а оператор на ней —
// уйти на дефолтную» были МЁРТВЫ: условие ложно всегда, и до вызова дело не
// доходило. Внешне — оператор остаётся на скрытом пункте меню (пустой экран).
//
// КАК ПРАВИЛЬНО. В фабриках проекта страница читается геттером `getCurrentPage()`
// (app-logs.js:20, app-data.js:42, app-models.js:37, app-settings.js:26; контракт
// закреплён в webui/tests/page-contract-r676.test.js). Этот модуль — синглтон, а не
// `window.X = function (ctx)`, поэтому те же два действия передаются ему явной
// привязкой из app.js (владельца состояния `currentPage`):
//   BackendTypeFilter.bindPage({ getCurrentPage: function () { return currentPage; },
//                                switchPage: switchPage });
//
// ЧТО ПРОВЕРЯЕТСЯ
//   1. поведение: страницу спрятали, оператор на ней — переход на dashboard ЕСТЬ;
//   2. оператор на другой странице — перехода НЕТ (ветка не «стреляет всегда»);
//   3. без привязки контракта — перехода нет и нет исключения (модуль не падает);
//   4. контракт читается ВЫЗОВОМ геттера, а не снимком; `window.ui.currentPage` /
//      `window.ui.switchPage` в боевом коде не читает НИКТО;
//   5. app.js действительно привязывает контракт (иначе п.1 мёртв в браузере).
//
// Запуск: node webui/tests/backend-type-filter-page-contract-r680.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const root = path.join(__dirname, '..');
const read = (rel) => fs.readFileSync(path.join(root, rel), 'utf8');
const moduleSrc = read('js/modules/backend-type-filter.js');
const appJs = read('js/app.js');

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

/** codeOf — исходник без комментариев: шапка модуля ЦИТИРУЕТ дефектный код. */
function codeOf(src) {
    const noBlock = src.replace(/\/\*[\s\S]*?\*\//g, '');
    return noBlock.split('\n').map(function (line) {
        const i = line.indexOf('//');
        return i === -1 ? line : line.slice(0, i);
    }).join('\n');
}

// ===========================================================================
// ПЕСОЧНИЦА
// ===========================================================================

function fakeNav(page) {
    return {
        dataset: { page: page },
        style: { display: '' },
        title: '',
        classList: { add: function () {}, remove: function () {}, contains: function () { return false; } }
    };
}

/**
 * loadModule — свежий экземпляр модуля в vm-контексте.
 *
 * Модуль — синглтон с состоянием (`pageApi`), поэтому проверка «без привязки»
 * требует отдельной загрузки: require() вернул бы уже привязанный экземпляр.
 */
function loadModule() {
    const navs = {
        gguf: fakeNav('gguf'),
        image: fakeNav('image'),
        agents: fakeNav('agents'),
        dashboard: fakeNav('dashboard')
    };
    const store = {};
    const doc = {
        querySelector: function (sel) {
            const m = /\.nav-item\[data-page="([^"]+)"\]/.exec(String(sel));
            return m ? (navs[m[1]] || null) : null;
        },
        querySelectorAll: function () { return []; },
        getElementById: function () { return null; },
        addEventListener: function () {}
    };
    const win = {
        document: doc,
        localStorage: null,
        I18N: { t: function (key) { return key; } },
        __lastClusterState: null
    };
    const sandbox = {
        window: win,
        document: doc,
        I18N: { t: function (key) { return key; } },
        console: { log: function () {}, warn: function () {}, error: function () {}, debug: function () {} },
        localStorage: {
            getItem: function (k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
            setItem: function (k, v) { store[k] = String(v); },
            removeItem: function (k) { delete store[k]; }
        }
    };
    vm.createContext(sandbox);
    vm.runInContext(moduleSrc, sandbox, { filename: 'backend-type-filter.js' });
    const F = win.BackendTypeFilter;
    assert.ok(F, 'модуль не выставил window.BackendTypeFilter');

    const switchedTo = [];
    return {
        F: F,
        navs: navs,
        switchedTo: switchedTo,
        /** bindPage — то, что делает app.js: оба действия ФУНКЦИЯМИ. */
        bindCurrentPage: function (holder) {
            return F.bindPage({
                getCurrentPage: function () { return holder.page; },
                switchPage: function (p) { switchedTo.push(p); holder.page = p; }
            });
        },
        /** clusterWith* — состав кластера, по которому считается видимость вкладок. */
        onlyImageCluster: function () {
            F._clusterTypeCounts = { image_cpp: 2 };
            F._serverEngineType = 'image_cpp';
        },
        onlyTextCluster: function () {
            F._clusterTypeCounts = { llama_cpp: 2 };
            F._serverEngineType = 'llama_cpp';
        }
    };
}

// ===========================================================================
// ПРОВЕРКИ
// ===========================================================================

check('R680/1: модуль публикует контракт страницы (bindPage)', function () {
    const env = loadModule();
    assert.strictEqual(typeof env.F.bindPage, 'function',
        'BackendTypeFilter.bindPage не опубликован — app.js не может передать геттер страницы');
    assert.strictEqual(env.F.bindPage(null), false, 'bindPage(null) обязан вернуть false');
    assert.strictEqual(env.F.bindPage({}), false, 'bindPage без методов ничего не принимает');
});

check('R680/1: страницу GGUF спрятали, оператор на ней — уходим на dashboard', function () {
    const env = loadModule();
    const holder = { page: 'gguf' };
    assert.strictEqual(env.bindCurrentPage(holder), true, 'контракт страницы не принят');
    env.onlyImageCluster();          // текстовых бэкендов нет → вкладка GGUF скрывается
    env.F.toggleGgufTab('image_cpp');

    assert.strictEqual(env.navs.gguf.style.display, 'none', 'вкладка GGUF не скрыта — сценарий не сложился');
    assert.deepStrictEqual(env.switchedTo, ['dashboard'],
        'оператор остался на скрытой странице GGUF: ветка перехода мертва (ровно дефект R680)');
    assert.strictEqual(holder.page, 'dashboard', 'переход не изменил текущую страницу');
});

check('R680/1: страницу Image спрятали, оператор на ней — уходим на dashboard', function () {
    const env = loadModule();
    const holder = { page: 'image' };
    env.bindCurrentPage(holder);
    env.onlyTextCluster();           // image-бэкендов нет → вкладка Image-моделей скрывается
    env.F.toggleImageTab();

    assert.strictEqual(env.navs.image.style.display, 'none', 'вкладка Image не скрыта — сценарий не сложился');
    assert.deepStrictEqual(env.switchedTo, ['dashboard'],
        'оператор остался на скрытой странице Image-моделей (ветка перехода мертва)');
});

check('R680/1: оператор на другой странице — перехода НЕТ', function () {
    const env = loadModule();
    const holder = { page: 'settings' };
    env.bindCurrentPage(holder);
    env.onlyImageCluster();
    env.F.toggleGgufTab('image_cpp');
    env.onlyTextCluster();
    env.F.toggleImageTab();

    assert.deepStrictEqual(env.switchedTo, [],
        'переход сработал, хотя оператор не на скрываемой странице: ветка «выстреливает всегда»');
    assert.strictEqual(holder.page, 'settings');
});

check('R680/1: без привязки контракта перехода нет (и нет исключения)', function () {
    const env = loadModule();
    env.onlyImageCluster();
    assert.doesNotThrow(function () { env.F.toggleGgufTab('image_cpp'); },
        'без привязки контракта модуль упал');
    assert.deepStrictEqual(env.switchedTo, [],
        'переход выполнен без привязки — откуда взялась страница?');
});

check('R680/1: страница читается ВЫЗОВОМ геттера, снимков и window.ui не осталось', function () {
    const code = codeOf(moduleSrc);
    assert.ok(!/window\.ui\s*\.\s*currentPage/.test(code),
        'модуль снова читает window.ui.currentPage: у `ui` нет этого свойства, ' +
        'условие будет ложным ВСЕГДА (дефект R680)');
    assert.ok(!/window\.ui\s*\.\s*switchPage/.test(code),
        'модуль снова читает window.ui.switchPage: этого метода в `ui` тоже нет');
    assert.ok(/pageApi\.getCurrentPage\s*\(\s*\)/.test(code),
        'геттер страницы не ВЫЗЫВАЕТСЯ — значение останется функцией (класс R676)');
    assert.ok(/ctx\.getCurrentPage/.test(code) && /ctx\.switchPage/.test(code),
        'bindPage не читает getCurrentPage/switchPage из контекста');
});

check('R680/1: window.ui.currentPage не читает НИ ОДИН боевой файл webui/js', function () {
    const files = [];
    (function walk(dir) {
        fs.readdirSync(dir, { withFileTypes: true }).forEach(function (entry) {
            const full = path.join(dir, entry.name);
            if (entry.isDirectory()) {
                if (entry.name === 'node_modules') return;
                walk(full);
                return;
            }
            if (!entry.name.endsWith('.js') || entry.name.endsWith('.test.js')) return;
            files.push(full);
        });
    })(path.join(root, 'js'));
    assert.ok(files.length > 50, 'обход боевых файлов нашёл всего ' + files.length + ' — проверка выродилась');
    const offenders = files.filter(function (f) {
        return /window\.ui\s*\.\s*(currentPage|switchPage)\b/.test(codeOf(fs.readFileSync(f, 'utf8')));
    }).map(function (f) { return path.relative(root, f).split(path.sep).join('/'); });
    assert.deepStrictEqual(offenders, [],
        'эти файлы читают несуществующие свойства window.ui (мёртвые ветки): ' + offenders.join(', '));
});

check('R680/1: app.js привязывает контракт страницы к BackendTypeFilter', function () {
    const code = codeOf(appJs);
    const bind = /BackendTypeFilter\s*\.\s*bindPage\s*\(\s*\{([\s\S]{0,400}?)\}\s*\)/.exec(code);
    assert.ok(bind, 'app.js не зовёт BackendTypeFilter.bindPage({…}) — в браузере контракт снова ' +
        'не привязан, и переход со скрытой страницы не работает (дефект R680)');
    const literal = bind[1];
    assert.ok(/getCurrentPage\s*:\s*function\s*\(\s*\)\s*\{\s*return\s+currentPage;\s*\}/.test(literal),
        'в литерале app.js нет геттера `getCurrentPage: function () { return currentPage; }` — ' +
        'страница обязана передаваться функцией, а не значением (её ещё нет на этой строке)');
    assert.ok(/switchPage\s*:\s*switchPage\b/.test(literal),
        'в литерале app.js не передан switchPage — уходить с исчезнувшей страницы будет некуда');
});

check('R680/1: модуль не читает страницу на загрузке (порядок скриптов не важен)', function () {
    // Привязка приходит из app.js позже; до неё геттер — null, и это нормально.
    const env = loadModule();
    assert.doesNotThrow(function () { env.F.toggleImageTab(); }, 'toggleImageTab до привязки упал');
    assert.deepStrictEqual(env.switchedTo, []);
});

if (failures.length) {
    console.error('\n' + failures.length + ' проверок провалено, ' + passed + ' пройдено');
    process.exit(1);
}
console.log('\nвсе ' + passed + ' проверок пройдены');
