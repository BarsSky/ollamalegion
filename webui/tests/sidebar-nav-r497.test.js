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

check('каждый пункт меню ведёт туда, где что-то есть (страница SPA или реальный файл)', () => {
    // R595 (2026-11-19): сторож переписан вместе со снятием механизма скрытия
    // пунктов меню. Раньше он требовал, чтобы каждая страница была в списках
    // `navModeDependent`/`navModeIndependent`; после W1 (режимы снесены,
    // `updateVariantSections` убрана) потребителей у списков не осталось, и они
    // удалены — а сторож проверял уже несуществующий механизм.
    //
    // Новый инвариант — тот самый дефект R497, ради которого сторож и появился:
    // «пункт меню ведёт в пустоту». Пункт обязан вести ЛИБО на SPA-страницу
    // (`#<page>` + `<div class="page" id="<page>-page">`), ЛИБО на реальный файл
    // (href не начинается с «#»): во втором случае клик отдаётся браузеру.
    const pages = pagesFromHtml();
    assert.ok(pages.size >= 13, 'в разметке найдено только ' + pages.size + ' страниц — сторож сломан');

    const empty = [];
    const re = /<a[^>]*data-page="([a-z0-9-]+)"[^>]*>/g;
    let m;
    while ((m = re.exec(html)) !== null) {
        const page = m[1];
        const href = (/href="([^"]*)"/.exec(m[0]) || [])[1] || '';
        if (href !== '' && href.charAt(0) !== '#') continue; // реальный файл — отдан браузеру
        if (html.indexOf('id="' + page + '-page"') === -1) empty.push(page);
    }
    assert.deepStrictEqual(empty, [],
        'пункты меню ведут в пустоту (нет секции #<page>-page и нет реального файла): ' +
        empty.join(', '));
});

check('R564 (W1): режимы удалены из UI, экспорт updateModeCards снят', () => {
    // БЫЛО (аудит §4.4): updateModeCards вызывался, но не экспортировался → TypeError.
    // СТАЛО (W1): режимы снесены (R560-R563), вызовов не осталось — проверяем, что
    // экспорт НЕ вернулся и что режимов нет в разметке.
    const exported = /window\.SettingsUI = \{[\s\S]*?updateModeCards: updateModeCards[\s\S]*?\};/.test(settingsUi);
    assert.strictEqual(exported, false,
        'updateModeCards снова экспортирован: режимы удалены, экспорт без потребителя — мёртвый код');
    const html = fs.readFileSync(path.join(__dirname, '..', 'index.html'), 'utf8');
    assert.strictEqual(html.indexOf('section-mode'), -1, 'секция режимов вернулась в разметку');
});

if (failures.length) {
    console.error('\nИТОГ: ' + failures.length + ' проверок красных');
    process.exit(1);
}
console.log('\nИТОГ: ' + passed + ' проверок зелёные');
