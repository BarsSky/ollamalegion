// webui/tests/form-dirty-guard.test.js — R662 (2026-11-20): незавершённый ввод
// защищает страницу от авто-обновления, а не только фокус в поле.
//
// ЖИВАЯ ЖАЛОБА (продолжение R512). «Постоянное обновление WebUI не даёт
// взаимодействовать с формами». R512 пропускал тик только пока document.activeElement —
// редактируемое поле активной страницы. Дырка: набрал значение → кликнул чекбокс или
// пустое место → фокус уже не в текстовом поле → следующий тик (раз в 5 с)
// перерисовывает страницу и затирает набранное.
//
// Проверяется: (1) модуль отличает правку от клика и знает, к какой странице она
// относится; (2) защита снимается сохранением (submit) и сама истекает по TTL —
// «заморозки навсегда» нет; (3) тик автообновления вызывает проверку ДО перерисовки;
// (4) модуль подключён в index.html (иначе window.FormDirty === undefined и проверка
// молча не работает — отдельный класс дефекта «код есть, не подключён»).
//
// Запуск: node webui/tests/form-dirty-guard.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const root = path.join(__dirname, '..');
const moduleSrc = fs.readFileSync(path.join(root, 'js', 'modules', 'form-dirty.js'), 'utf8');
const appDataJs = fs.readFileSync(path.join(root, 'js', 'modules', 'app-data.js'), 'utf8');
const indexHtml = fs.readFileSync(path.join(root, 'index.html'), 'utf8');

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

// Загружаем модуль так же, как в браузере: sandbox с window/document.
function loadModule() {
    const sandboxWindow = {};
    const listeners = {};
    const sandboxDocument = {
        addEventListener: function (type, fn) { listeners[type] = fn; },
        activeElement: null,
        body: { id: 'body', parentNode: null }
    };
    const sandbox = {
        window: sandboxWindow,
        document: sandboxDocument,
        Date: Date,
        module: { exports: {} }
    };
    vm.createContext(sandbox);
    vm.runInContext(moduleSrc, sandbox, { filename: 'form-dirty.js' });
    assert.ok(sandboxWindow.FormDirty, 'модуль form-dirty.js не выставил window.FormDirty');
    return { api: sandboxWindow.FormDirty, listeners, sandbox };
}

// Фейковый DOM-узел: pageIdOf достаточно id/parentNode/tagName/type.
function node(tagName, props) {
    return Object.assign({ tagName: tagName }, props || {});
}
function page(id) {
    return node('DIV', { id: id, parentNode: null });
}

// --- (1) правка внутри страницы против клика по кнопке ---
check('правка в поле страницы помечает страницу', () => {
    const { api } = loadModule();
    const settingsPage = page('settings-page');
    const input = node('INPUT', { type: 'text', id: 'cfg-host', parentNode: settingsPage });
    assert.strictEqual(api.isDirtyPage('settings'), false, 'до правки страница чистая');
    api.noteEdit(input);
    assert.strictEqual(api.isDirtyPage('settings'), true, 'после правки страница «грязная»');
    assert.strictEqual(api.isDirtyPage('settings-page'), true, 'форма с суффиксом -page тоже распознаётся');
});

check('клик по кнопке (не редактируемый элемент) страницу не помечает', () => {
    const { api } = loadModule();
    const settingsPage = page('settings-page');
    const button = node('BUTTON', { id: 'saveSettings', parentNode: settingsPage });
    assert.strictEqual(api.isEditable(button), false, 'кнопка не считается вводом');
    assert.strictEqual(api.isDirtyPage('settings'), false, 'без правки защиты нет');
});

check('правка вне страницы (нет контейнера -page) не помечает ничего', () => {
    const { api } = loadModule();
    const stray = node('INPUT', { type: 'text', id: 'search', parentNode: null });
    assert.strictEqual(api.pageIdOf(stray), '');
    assert.strictEqual(api.noteEdit(stray), '');
});

// --- (2) сброс защиты: submit и TTL ---
check('submit формы снимает защиту сразу', () => {
    const { api } = loadModule();
    const settingsPage = page('settings-page');
    const input = node('INPUT', { type: 'text', parentNode: settingsPage });
    api.noteEdit(input);
    assert.strictEqual(api.isDirtyPage('settings'), true);
    api.noteSave(settingsPage);
    assert.strictEqual(api.isDirtyPage('settings'), false, 'после сохранения защита не нужна');
});

check('защита истекает по TTL (авто-обновление возвращается само)', () => {
    const { api } = loadModule();
    const settingsPage = page('settings-page');
    const input = node('INPUT', { type: 'text', parentNode: settingsPage });
    let now = 1000;
    api.setNowForTest(() => now);
    api.setTTLForTest(5000);
    api.noteEdit(input);
    now = 1000 + 4000;
    assert.strictEqual(api.isDirtyPage('settings'), true, 'в пределах TTL защита действует');
    now = 1000 + 6000;
    assert.strictEqual(api.isDirtyPage('settings'), false, 'после TTL тик снова работает');
});

check('clearAll сбрасывает состояние (навигация по страницам)', () => {
    const { api } = loadModule();
    const settingsPage = page('settings-page');
    const input = node('INPUT', { type: 'text', parentNode: settingsPage });
    api.noteEdit(input);
    api.clearAll();
    assert.strictEqual(api.isDirtyPage('settings'), false);
});

// --- (3) тик автообновления реально спрашивает про несохранённый ввод ---
check('тик проверяет isDirtyPage ДО перерисовки страницы', () => {
    const dirtyIdx = appDataJs.indexOf('FormDirty.isDirtyPage');
    assert.ok(dirtyIdx > 0, 'в app-data.js нет вызова window.FormDirty.isDirtyPage — защита не подключена');
    const refreshIdx = appDataJs.indexOf('DR.refresh(currentPage)');
    assert.ok(refreshIdx > 0, 'в app-data.js не найден вызов DR.refresh(currentPage) — разметка тика изменилась');
    assert.ok(dirtyIdx < refreshIdx, 'проверка незавершённого ввода стоит ПОСЛЕ перерисовки');
});

// --- (4) модуль подключён в index.html ---
check('index.html подключает form-dirty.js', () => {
    assert.ok(indexHtml.indexOf('js/modules/form-dirty.js') > 0,
        'index.html не подключает js/modules/form-dirty.js — window.FormDirty будет undefined');
    const guardIdx = indexHtml.indexOf('js/modules/refresh-guard.js');
    const dirtyIdx = indexHtml.indexOf('js/modules/form-dirty.js');
    assert.ok(guardIdx > 0 && dirtyIdx > guardIdx,
        'form-dirty.js должен идти ПОСЛЕ refresh-guard.js (логика страниц — единым блоком)');
});

if (failures.length) {
    console.error('\n' + failures.length + ' проверок провалено, ' + passed + ' пройдено');
    process.exit(1);
}
console.log('\nвсе ' + passed + ' проверок пройдены');
