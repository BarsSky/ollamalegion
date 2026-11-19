// sidebar-nav-r497.test.js — R497 (2026-11-19): сайдбар обязан сохранять ВСЕ
// страницы, объявленные в разметке.
//
// ЖИВОЙ ДЕФЕКТ (аудит consistency-audit §4.1; наблюдение владельца: «страницы на
// краткий миг появляются и пропадают — видно только при обновлении со сбросом
// кэша»). `SettingsUI.updateVariantSections(mode)` прячет ЛЮБОЙ `.nav-item`,
// которого нет в списке своей копии `navMap`. Копий было ПЯТЬ (standard,
// replication, rpc_coordinator, virtual_router, distributed_inference), и ни в
// одной не было `diagnostics`, `reference`, `health`, `virtual-models`. То есть
// после первого же рендера режима (страница «Настройки» или мастер) из сайдбара
// исчезали «Диагностика» и «Справка и шаблоны». Обратно их не показывал никто:
// `BackendTypeFilter.applyChrome()` восстанавливает только gguf/image.
//
// Почему не поймал существующий тест: `backend-type-filter-nav-r94.test.js`
// мокает ЧЕТЫРЕ пункта (`gguf`, `image`, `agents`, `backend`) и про режимные
// списки ничего не знает. Этот сторож сверяет РАЗМЕТКУ с КОДОМ: каждая страница
// из `index.html` обязана быть в списках видимости.
//
// Запуск: node webui/tests/sidebar-nav-r497.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const root = path.join(__dirname, '..');
const read = (rel) => fs.readFileSync(path.join(root, rel), 'utf8');

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

const html = read('index.html');
const settingsUi = read('js/modules/settings-ui.js');

// pagesFromHtml — все страницы, объявленные пунктами меню.
function pagesFromHtml() {
    const re = /class="[^"]*nav-item[^"]*"[^>]*data-page="([a-z0-9-]+)"/g;
    const found = new Set();
    let m;
    while ((m = re.exec(html)) !== null) { found.add(m[1]); }
    // data-page может стоять до class в разметке — второй проход по <a ...>.
    const re2 = /<a[^>]*data-page="([a-z0-9-]+)"[^>]*class="[^"]*nav-item[^"]*"/g;
    while ((m = re2.exec(html)) !== null) { found.add(m[1]); }
    return found;
}

// navListsFromJs — массивы navModeDependent + navModeIndependent.
function navListsFromJs() {
    const dep = /var navModeDependent = \[([\s\S]*?)\];/.exec(settingsUi);
    const indep = /var navModeIndependent = \[([\s\S]*?)\];/.exec(settingsUi);
    assert.ok(dep, 'в settings-ui.js не найден navModeDependent — сторож потерял точку отсчёта');
    assert.ok(indep, 'в settings-ui.js не найден navModeIndependent — сторож потерял точку отсчёта');
    const parse = (src) => new Set((src.match(/'[a-z0-9-]+'/g) || []).map((s) => s.replace(/'/g, '')));
    const all = parse(dep[1]);
    parse(indep[1]).forEach((p) => all.add(p));
    return all;
}

check('каждая страница из index.html есть в списках видимости сайдбара', () => {
    const pages = pagesFromHtml();
    assert.ok(pages.size >= 13, 'в разметке найдено только ' + pages.size + ' страниц — сторож сломан');
    const allowed = navListsFromJs();
    const missing = [...pages].filter((p) => !allowed.has(p));
    assert.deepStrictEqual(missing, [],
        'эти страницы есть в разметке, но НЕ в списках видимости — после рендера режима ' +
        'они исчезнут из сайдбара (живой дефект R497): ' + missing.join(', '));
});

check('список режимных страниц не размножен по копиям', () => {
    // Копии списка из пяти строк — причина дефекта: добавили страницу в одну
    // копию и забыли четыре. Сторож следит, чтобы копий снова не появилось.
    const copies = /show: \['dashboard'/g;
    const count = (settingsUi.match(copies) || []).length;
    assert.strictEqual(count, 0,
        'найдено ' + count + ' копий(и) списка страниц: список должен собираться один раз ' +
        '(navModeDependent + navModeIndependent), иначе новая страница снова «потеряется»');
});

check('updateModeCards экспортирован и вызывается снаружи', () => {
    const exported = /window\.SettingsUI = \{[\s\S]*?updateModeCards: updateModeCards[\s\S]*?\};/.test(settingsUi);
    assert.ok(exported,
        'updateModeCards не экспортирован из SettingsUI — снаружи будет TypeError ' +
        '(аудит §4.4), и applyServerConfig не обновит часть настроек');

    const callers = ['js/app.js', 'js/modules/config-io.js'];
    for (const file of callers) {
        const src = read(file);
        assert.ok(/SettingsUI\.updateModeCards\s*\(/.test(src),
            file + ': вызов SettingsUI.updateModeCards пропал — проверка потеряла смысл');
    }
});

if (failures.length) {
    console.error('\nИТОГ: ' + failures.length + ' проверок красных');
    process.exit(1);
}
console.log('\nИТОГ: ' + passed + ' проверок зелёные');
