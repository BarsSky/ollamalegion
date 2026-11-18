// health-css-tokens.test.js — R514 (2026-11-19): страница «Состояние» обязана
// рисовать рамки, то есть не использовать переменные, которых нет в её стилях.
//
// ЖИВОЙ ДЕФЕКТ. В webui/health.html семь объявлений `border: 1px solid
// var(--border-color)`. Переменная `--border-color` определена ТОЛЬКО в
// css/monitor-app.css (и равна там `var(--border)`), а health.html подключает
// font-awesome/themes/base/components/pages/layout. Невалидное значение внутри
// `border` отбрасывает ВСЁ объявление, поэтому у карточек, секций, строк таблиц
// и кнопок границ не было вовсе — оператор видел «чужие стили, не интегрирована».
//
// Сторож не про одну переменную: он берёт СПИСОК стилей, которые страница
// реально грузит, собирает из них определения переменных и требует, чтобы
// каждая `var(--x)` из разметки (включая inline style-атрибуты) была определена.
// Так же ловятся `var(--bg)`/`var(--text)` из будущих правок.
//
// Запуск: node webui/tests/health-css-tokens.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const root = path.join(__dirname, '..');
const read = (rel) => fs.readFileSync(path.join(root, rel), 'utf8');
const html = read('health.html');

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

// linkedStylesheets — css-файлы, подключённые страницей (без ?v=).
function linkedStylesheets() {
    const out = [];
    const re = /<link[^>]+rel="stylesheet"[^>]+href="([^"]+\.css)(?:\?[^"]*)?"/g;
    let m;
    while ((m = re.exec(html)) !== null) out.push(m[1]);
    return out;
}

// definedVars — какие `--name:` определены в перечисленных css-файлах.
function definedVars(files) {
    const defs = new Set();
    files.forEach((rel) => {
        const css = read(rel);
        const re = /(--[a-z0-9-]+)\s*:/gi;
        let m;
        while ((m = re.exec(css)) !== null) defs.add(m[1]);
    });
    return defs;
}

// usedVars — какие `var(--name)` страница использует: без учёта fallback
// (если fallback есть, объявление валидно и без определения переменной).
function usedVars() {
    const used = new Set();
    const safe = new Set();
    const re = /var\(\s*(--[a-z0-9-]+)\s*(,)?/gi;
    let m;
    while ((m = re.exec(html)) !== null) {
        if (m[2] === ',') safe.add(m[1]);
        else used.add(m[1]);
    }
    return { used, safe };
}

const sheets = linkedStylesheets();
const defs = definedVars(sheets);
const { used } = usedVars();

check('R514: страница грузит стили (иначе проверять нечего)', () => {
    assert.ok(sheets.length >= 4, 'подключено css-файлов: ' + sheets.length + ' — ' + sheets.join(', '));
});

check('R514: каждая переменная из health.html определена в её стилях', () => {
    const missing = [...used].filter((v) => !defs.has(v));
    assert.deepStrictEqual(missing, [],
        'эти переменные НЕ определены в подключённых стилях — объявления с ними ' +
        'отбрасываются браузером (именно так не рисовались рамки): ' + missing.join(', '));
});

check('R514: --border-color (определён только в monitor-app.css) больше не используется', () => {
    assert.ok(html.indexOf('var(--border-color)') === -1,
        'страница снова ссылается на --border-color, которого в её стилях нет');
    assert.ok(html.indexOf('var(--border)') !== -1,
        'страница не использует общий токен --border — рамки снова могут пропасть');
});

check('R514: рамки заданы у карточек и секций (положительный якорь)', () => {
    ['.health-card {', '.health-section {'].forEach((anchor) => {
        const idx = html.indexOf(anchor);
        assert.ok(idx !== -1, 'не найдено правило ' + anchor);
        const line = html.slice(idx, html.indexOf('}', idx));
        assert.ok(/border:\s*1px solid var\(--border\)/.test(line),
            'у ' + anchor + ' нет валидной рамки: ' + line.trim());
    });
});

check('R514: у health.html нет стилей из monitor-app.css (их там не грузят)', () => {
    assert.ok(sheets.indexOf('css/monitor-app.css') === -1,
        'страница начала грузить monitor-app.css — проверка выше потеряла смысл');
});

if (failures.length) {
    console.error('\nИТОГ: ' + failures.length + ' проверок красных');
    process.exit(1);
}
console.log('\nИТОГ: ' + passed + ' проверок зелёные');
