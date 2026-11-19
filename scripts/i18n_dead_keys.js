#!/usr/bin/env node
/**
 * i18n_dead_keys.js — R570 (W7-хвост): какие ключи словарей НЕ ИСПОЛЬЗУЮТСЯ.
 *
 * ЗАЧЕМ ОТДЕЛЬНЫЙ ИНСТРУМЕНТ, А НЕ РЕГЕКС НА КОЛЕНКЕ. Первая попытка (R568) искала
 * только `I18N.t('…')` и `data-i18n="…"` в .html и объявила «мёртвыми» 1783 ключа из
 * 2574. Это были ЛОЖНЫЕ срабатывания: модули зовут локальные обёртки
 * (`diagnostics.js:20` — `function t(key, fallback)` → `t('diag.col_model')`) и
 * собирают `data-i18n` внутри template-строк. Удаление по тому списку вырезало бы
 * живые переводы.
 *
 * ЧТО УМЕЕТ:
 *   1. `data-i18n`, `data-i18n-title`, `data-i18n-placeholder="key"` — в .html И в .js
 *      (template-строки модулей);
 *   2. `I18N.t('key')` / `I18N.t("key")`;
 *   3. локальные обёртки, объявленные в файле, — понимает ЛЮБОЕ объявление вида
 *      `function <name>(key…` и считает вызовы `<name>('key')` в этом же файле
 *      (так ловится `t(`, `tr(`, `_t(` и подобные);
 *   4. `key: 'literal'` в объектах правил (`{ name: 'x', key: 'settings.…' }`) —
 *      встречается в реестрах настроек.
 *
 * ЧЕГО НЕ УМЕЕТ (и поэтому НЕ удаляет ничего сам): динамическую сборку ключей
 * (`'settings.' + name`, шаблоны с ${…}). Такие ключи попадут в отчёт как
 * «возможно мёртвые»; решение по ним — за человеком, с allowlist.
 *
 * ЗАПУСК: node scripts/i18n_dead_keys.js [--json]
 * Код возврата всегда 0: это ИЗМЕРИТЕЛЬ, а не страж (страж появится, когда список
 * неиспользуемых станет пустым и его можно будет зафиксировать).
 */
'use strict';

const fs = require('fs');
const path = require('path');

const ROOT = path.join(__dirname, '..');
const WEBUI = path.join(ROOT, 'webui');

function walk(dir, out, filter) {
    for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
        const full = path.join(dir, e.name);
        if (e.isDirectory()) {
            if (e.name === 'node_modules' || e.name === 'webfonts') continue;
            walk(full, out, filter);
            continue;
        }
        if (filter(e.name)) out.push(full);
    }
    return out;
}

// --- ключи словаря --------------------------------------------------------
const ruSrc = fs.readFileSync(path.join(WEBUI, 'js/i18n/ru.js'), 'utf8');
const enSrc = fs.readFileSync(path.join(WEBUI, 'js/i18n/en.js'), 'utf8');
const keyRe = /^\s*"([A-Za-z0-9_.]+)"\s*:/gm;
function keysOf(src) {
    const out = new Set();
    let m;
    while ((m = keyRe.exec(src)) !== null) out.add(m[1]);
    return out;
}
const ruKeys = keysOf(ruSrc);
const enKeys = keysOf(enSrc);

// --- потребители ----------------------------------------------------------
const used = new Set();
const dynamicHints = [];

function collectFromSource(src, rel) {
    // 1. атрибуты data-i18n*
    for (const m of src.matchAll(/data-i18n(?:-title|-placeholder|-aria-label)?="([A-Za-z0-9_.]+)"/g)) {
        used.add(m[1]);
    }
    // 2. прямой вызов I18N.t / window.I18N.t
    for (const m of src.matchAll(/(?:window\.)?I18N\.t\(\s*['"]([A-Za-z0-9_.]+)['"]/g)) {
        used.add(m[1]);
    }
    // 3. локальные обёртки: объявления `function <name>(key` в этом файле
    const wrappers = new Set();
    for (const m of src.matchAll(/function\s+([A-Za-z_$][\w$]*)\s*\(\s*key\b/g)) {
        wrappers.add(m[1]);
    }
    for (const name of wrappers) {
        const callRe = new RegExp('(?:^|[^\\w$.])' + name.replace(/\$/g, '\\$') + '\\(\\s*[\'"]([A-Za-z0-9_.]+)[\'"]', 'g');
        for (const m of src.matchAll(callRe)) used.add(m[1]);
    }
    // 4. `key: 'literal'` в объектах правил
    for (const m of src.matchAll(/key:\s*['"]([A-Za-z0-9_.]+\.[A-Za-z0-9_.]+)['"]/g)) {
        used.add(m[1]);
    }
    // 5. динамика: 'prefix.' + … — фиксируем префиксы как «возможно используемые»
    for (const m of src.matchAll(/['"]([A-Za-z0-9_.]+\.)['"]\s*\+/g)) {
        dynamicHints.push({ file: rel, prefix: m[1] });
    }
}


// --- ДОПОЛНИТЕЛЬНОЕ ПРАВИЛО (R574): любая строка, РАВНАЯ ключу словаря ----------
//
// ЗАЧЕМ. Первая версия искала конкретные шаблоны вызова и потому зависела от формы:
// обёртка, объявленная в другом файле, объект-обёртка (`{t: I18N.t}`), локальная
// копия словаря — всё это давало ложные «мёртвые». Для РЕШЕНИЯ ОБ УДАЛЕНИИ ошибка в
// эту сторону опасна (удалим живой перевод), поэтому правило переворачивается:
// ключ считается использованным, если его ТОЧНАЯ строка встречается в любом боевом
// JS/HTML. Переоценка здесь допустима: она оставляет ключ живым, а не убивает живой.
function markLiteralUses(allText) {
    for (const k of ruKeys) {
        if (used.has(k)) continue;
        if (allText.indexOf("'" + k + "'") !== -1 || allText.indexOf('"' + k + '"') !== -1) {
            used.add(k);
        }
    }
}
const htmlFiles = walk(WEBUI, [], (n) => n.endsWith('.html'));
const jsFiles = walk(path.join(WEBUI, 'js'), [], (n) => n.endsWith('.js') && !n.endsWith('.test.js'));

for (const f of htmlFiles.concat(jsFiles)) {
    collectFromSource(fs.readFileSync(f, 'utf8'), path.relative(ROOT, f).replace(/\\/g, '/'));
}

// R574: любая строка, равная ключу, считается использованием (см. markLiteralUses).
let allText = '';
// ВАЖНО (найдено на живом прогоне): словари ИСКЛЮЧЕНЫ. Иначе каждый ключ
// "находится" в самом файле словаря и правило считает живым всё.
for (const f of htmlFiles.concat(jsFiles)) {
    if (path.relative(WEBUI, f).split(path.sep).indexOf("i18n") !== -1) continue;
    allText += fs.readFileSync(f, 'utf8') + '\\n';
}
markLiteralUses(allText);

// Ключ, начинающийся с зафиксированного динамического префикса, считаем используемым.
const prefixes = [...new Set(dynamicHints.map((h) => h.prefix))];
function coveredByPrefix(key) {
    return prefixes.some((p) => key.startsWith(p));
}

const dead = [];
const possibleDynamic = [];
for (const k of ruKeys) {
    if (used.has(k)) continue;
    if (coveredByPrefix(k)) { possibleDynamic.push(k); continue; }
    dead.push(k);
}

const missingRu = [...enKeys].filter((k) => !ruKeys.has(k));
const missingEn = [...ruKeys].filter((k) => !enKeys.has(k));

const report = {
    keys: { ru: ruKeys.size, en: enKeys.size },
    used: used.size,
    unused: dead.length,
    possiblyDynamic: possibleDynamic.length,
    dynamicPrefixes: prefixes.length,
    missingInRu: missingRu.length,
    missingInEn: missingEn.length,
};

if (process.argv.includes('--json')) {
    console.log(JSON.stringify({ ...report, unusedKeys: dead }, null, 2));
    process.exit(0);
}

console.log('i18n: ключи и потребители');
console.log('  ключей: ru=%d en=%d', report.keys.ru, report.keys.en);
console.log('  использований найдено: %d', report.used);
console.log('  НЕ использовано: %d', report.unused);
console.log('  под динамическим префиксом (не считаем мёртвыми): %d (префиксов: %d)',
    report.possiblyDynamic, report.dynamicPrefixes);
console.log('  расхождение словарей: нет в ru=%d, нет в en=%d', report.missingInRu, report.missingInEn);
console.log('\nПервые 20 неиспользуемых (только для разбора человеком, НЕ для удаления):');
dead.slice(0, 20).forEach((k) => console.log('  ' + k));
console.log('\nВывод: список — ГИПОТЕЗА. Динамические ключи и локальные обёртки, объявленные');
console.log('не в том файле, где вызываются, могут попасть сюда ошибочно. Удалять только с allowlist.');
