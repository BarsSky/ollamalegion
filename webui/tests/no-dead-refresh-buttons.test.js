// no-dead-refresh-buttons.test.js — R84 (2026-10-03): защита от «мёртвых» кнопок
// обновления.
//
// ЗАЧЕМ. Оператор сообщил: «слишком много кнопок обновить на страницах, и часть
// из них не производят обновление». Проверка живого стенда подтвердила: кнопка
// #imgTestRefreshBtn существовала в разметке, НЕ имела ни одного обработчика в JS
// и по клику не делала ни одного запроса. Такое ловится только статически:
// обычные unit-тесты модулей про разметку ничего не знают.
//
// ЧТО ПРОВЕРЯЕТ:
//   1. каждый элемент обновления в index.html либо привязан к обработчику в JS
//      (getElementById/byId/el/querySelector), либо работает через делегирование
//      data-*-action, либо внесён в ALLOWED с объяснением;
//   2. страничные дубли кнопки «Обновить» больше не вернулись: их заменяет
//      индикатор свежести в шапке + провайдеры страниц (data-refresh.js).
//
// Запуск: node webui/tests/no-dead-refresh-buttons.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const root = path.join(__dirname, '..');
const indexHtml = path.join(root, 'index.html');

// Элементы, у которых обработчик живёт по другому принципу — с объяснением,
// почему это не «мёртвая кнопка».
const ALLOWED = {
    modelOpsAutoRefresh: 'не кнопка, а индикатор авто-обновления статуса операций (span)',
};

function jsSources() {
    const out = [];
    (function walk(dir) {
        for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
            const p = path.join(dir, e.name);
            if (e.isDirectory()) { if (e.name !== 'tests') walk(p); }
            else if (e.name.endsWith('.js') && !e.name.endsWith('.test.js')) out.push(p);
        }
    })(path.join(root, 'js'));
    return out.map((f) => ({ file: path.relative(root, f), text: fs.readFileSync(f, 'utf8') }));
}

function boundBy(id, sources) {
    const patterns = [
        new RegExp("getElementById\\(['\"]" + id + "['\"]\\)"),
        new RegExp("byId\\(['\"]" + id + "['\"]\\)"),
        new RegExp("el\\(['\"]" + id + "['\"]\\)"),
        new RegExp("querySelector\\(['\"]#" + id + "['\"]\\)"),
    ];
    return sources.filter((s) => patterns.some((re) => re.test(s.text))).map((s) => s.file);
}

const html = fs.readFileSync(indexHtml, 'utf8');
const sources = jsSources();

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

function refreshControls() {
    const found = [];
    html.split(/\r?\n/).forEach((line, i) => {
        // Кнопки/ссылки с id, в котором есть refresh, и кнопки с делегированным
        // data-*-action, тоже про refresh.
        const id = line.match(/id="([^"]*[Rr]efresh[^"]*)"/);
        const action = line.match(/data-(?:imh|imt|action)-?action?="([^"]*refresh[^"]*)"/) ||
            line.match(/data-(?:imh|imt)-action="([^"]*refresh[^"]*)"/);
        if (id) found.push({ line: i + 1, id: id[1], delegated: !!action, snippet: line.trim() });
        else if (action) found.push({ line: i + 1, id: '(delegated) ' + action[1], delegated: true, snippet: line.trim() });
    });
    return found;
}

const controls = refreshControls();

check('в index.html есть элементы обновления (иначе тест бессмыслен)', function () {
    assert.ok(controls.length >= 3, 'найдено всего ' + controls.length + ' элементов обновления');
});

check('каждый элемент обновления привязан к обработчику или объяснён в ALLOWED', function () {
    const dead = [];
    for (const c of controls) {
        if (c.delegated) continue;                      // работает через делегирование
        const id = c.id.replace(/^\(delegated\) /, '');
        if (ALLOWED[id]) continue;
        if (boundBy(id, sources).length === 0) dead.push('L' + c.line + ' #' + id);
    }
    assert.deepStrictEqual(dead, [], 'кнопки без обработчика (клик ничего не делает): ' + dead.join(', '));
});

check('страничные дубли кнопки «Обновить» удалены (их роль — индикатор свежести)', function () {
    const removed = [
        'refreshModelsBtn', 'refreshAgentsBtn', 'refreshProxyLogs',
        'imgRefreshBtn', 'imageBackendsRefresh', 'imageProfilesRefreshBtn', 'imgTestRefreshBtn',
    ];
    const back = removed.filter((id) => html.indexOf('id="' + id + '"') !== -1);
    assert.deepStrictEqual(back, [],
        'вернулись дублирующие кнопки: ' + back.join(', ') +
        ' — они должны были быть заменены индикатором #dataFreshness и провайдерами страниц (data-refresh.js)');
});

check('в шапке есть ровно одна кнопка обновления + индикатор свежести и пауза', function () {
    assert.ok(html.indexOf('id="dataFreshness"') !== -1, 'нет индикатора свежести #dataFreshness');
    assert.ok(html.indexOf('id="dataFreshnessText"') !== -1, 'нет подписи индикатора');
    assert.ok(html.indexOf('id="dataAutoToggle"') !== -1, 'нет переключателя авто-обновления');
    const topLevel = (html.match(/id="refreshBtn"/g) || []).length;
    assert.strictEqual(topLevel, 1, 'кнопок #refreshBtn должно быть ровно одна, найдено ' + topLevel);
});

check('data-refresh.js и его провайдеры подключены в index.html', function () {
    assert.ok(html.indexOf('js/modules/data-refresh.js') !== -1, 'data-refresh.js не подключён');
    assert.ok(html.indexOf('js/modules/gguf-page-provider.js') !== -1, 'gguf-page-provider.js не подключён');
    // Порядок важен: app.js в init() вызывает DataRefresh.mount() и регистрирует
    // провайдеров, поэтому модуль обязан грузиться раньше.
    const dr = html.indexOf('js/modules/data-refresh.js');
    const app = html.indexOf('js/app.js');
    assert.ok(dr !== -1 && app !== -1 && dr < app, 'data-refresh.js должен грузиться ДО app.js');
});

check('каждая страница с собственным источником имеет провайдера', function () {
    const appJs = fs.readFileSync(path.join(root, 'js', 'app.js'), 'utf8');
    const required = ['models', 'agents', 'logs', 'sessions', 'queue', 'monitor', 'settings'];
    const missing = required.filter((p) => appJs.indexOf("DR.register('" + p + "'") === -1);
    assert.deepStrictEqual(missing, [], 'нет провайдеров для страниц: ' + missing.join(', '));
    // image/gguf регистрируются в своих модулях.
    const imgPage = fs.readFileSync(path.join(root, 'js', 'modules', 'image-page.js'), 'utf8');
    assert.ok(imgPage.indexOf("DataRefresh.register('image'") !== -1, 'image-page не регистрирует провайдера');
    const ggufProv = fs.readFileSync(path.join(root, 'js', 'modules', 'gguf-page-provider.js'), 'utf8');
    assert.ok(ggufProv.indexOf("register('gguf'") !== -1, 'gguf-page-provider не регистрирует провайдера');
});

console.log('\n' + (failures.length ? 'FAILED: ' + failures.length : 'ИТОГ: все проверки пройдены (' + passed + ')'));
if (failures.length) {
    failures.forEach(function (f) { console.error(' - ' + f); });
    process.exit(1);
}
