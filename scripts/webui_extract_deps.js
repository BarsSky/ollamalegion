#!/usr/bin/env node
/**
 * webui_extract_deps.js — какие имена нужны вынесенному куску JS из его прежнего файла.
 *
 * ЗАЧЕМ (R644, 2026-11-19). При выносе слоёв из WebUI-модулей я три раза подряд
 * ошибся ОДИНАКОВО: собрал список зависимостей «на глаз по function-объявлениям» и
 * забыл то, что объявлено иначе. Живые примеры этого цикла:
 *
 *   R640 reference-page.js  → забыт `backendChoices` (функция была в списке, но не в
 *                             экспорте) → `ReferenceError: backendChoices is not defined`;
 *   R643 renderers.js        → забын `escapeHtml` — он приходит из ДЕСТРУКТУРИЗАЦИИ
 *                             `Utils`, а не объявлением `function` → ReferenceError;
 *   R643 renderers.js        → забын `OLLAMA_PARAMS` — это `const`-таблица →
 *                             `ReferenceError: OLLAMA_PARAMS is not defined`.
 *
 * Каждая ошибка ловилась тестом (это хорошо), но каждая стоила круга правок (это
 * плохо). Скрипт отвечает на вопрос механически: берёт текст файла и список имён
 * вынесенных функций, вырезает их тела и печатает имена, которые им нужны СНАРУЖИ —
 * включая объявленные через `const`, `let`, `var`, деструктуризацию и параметры
 * фабрик.
 *
 * ЧЕГО СКРИПТ НЕ ДЕЛАЕТ: он не отличает «имя объявлено в этом файле» от «глобал
 * браузера» — список нужен, чтобы ничего не забыть при передаче контекста, а не
 * чтобы доказать отсутствие зависимостей. Лишнее имя в списке безвредно.
 *
 * ЗАПУСК:
 *   node scripts/webui_extract_deps.js <файл.js> <имя1> [имя2 ...] [--json]
 *
 * ПРИМЕР (R643):
 *   node scripts/webui_extract_deps.js webui/js/modules/renderers.js \
 *        renderImageParams renderOllamaParams formatAgeR348 formatLoadedAtR346
 *   → { "deps": ["OLLAMA_PARAMS", "Utils", "_t", "escapeHtml", "formatMB", ...] }
 */
'use strict';

const fs = require('fs');
const path = require('path');

// --- разбор файла ---------------------------------------------------------

/** declaredNames — все имена, объявленные в файле: function/const/let/var и
 *  деструктуризация (`const { a, b } = Utils;` → a, b). */
function declaredNames(src) {
    const names = new Set();
    for (const m of src.matchAll(/\bfunction\s+([A-Za-z_$][\w$]*)\s*\(/g)) names.add(m[1]);
    // const/let/var X = …
    for (const m of src.matchAll(/\b(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=/g)) names.add(m[1]);
    // const { a, b: c, ...rest } = …
    for (const m of src.matchAll(/\b(?:const|let|var)\s*\{([^}]*)\}\s*=/g)) {
        m[1].split(',').forEach(function (part) {
            const t = part.trim();
            if (!t) return;
            const renamed = t.split(':').pop().trim();
            const id = (renamed.match(/^[A-Za-z_$][\w$]*/) || [])[0];
            if (id) names.add(id);
        });
    }
    // параметры функций (`function f(Utils, isZeroTimestamp)` и стрелочные — не нужны:
    // нас интересуют имена, которые приходят извне, а параметры уже локальны).
    return names;
}

/** bodyOf — текст функции по имени: от `function name(` до парной закрывающей `}`.
 *  Скобки считаем балансом, строки и комментарии пропускаем, чтобы не сбиться на
 *  `}` внутри строки. */
function bodyOf(src, name) {
    const re = new RegExp('\\bfunction\\s+' + name.replace(/\$/g, '\\$') + '\\s*\\(');
    const m = re.exec(src);
    if (!m) return null;
    let i = src.indexOf('{', m.index + m[0].length);
    if (i < 0) return null;
    let depth = 0;
    let inS = null, inC = null, inT = null;
    for (; i < src.length; i++) {
        const c = src[i], prev = src[i - 1], next = src[i + 1];
        if (inC) { if (c === '\n') inC = null; continue; }
        if (inS) { if (c === '\\') { i++; continue; } if (c === inS) inS = null; continue; }
        if (inT) { if (c === '\\') { i++; continue; } if (c === '`') inT = null; continue; }
        if (c === '/' && next === '/') { inC = c; continue; }
        if (c === '/' && next === '*') { const end = src.indexOf('*/', i + 2); i = end < 0 ? src.length : end + 1; continue; }
        if (c === '"' || c === "'") { inS = c; continue; }
        if (c === '`') { inT = c; continue; }
        if (c === '{') depth++;
        else if (c === '}') { depth--; if (depth === 0) return src.slice(m.index, i + 1); }
    }
    return null;
}

/** depsOf — имена, которые нужны переданным функциям, но объявлены вне их тел. */
function depsOf(src, funcNames) {
    const all = declaredNames(src);
    const bodies = [];
    for (const fn of funcNames) {
        const b = bodyOf(src, fn);
        if (!b) throw new Error('функция не найдена в файле: ' + fn);
        bodies.push(b);
    }
    const text = bodies.join('\n');
    // локальные объявления внутри вынесенных тел — не зависимость
    const innerDecl = new Set();
    for (const b of bodies) {
        for (const n of declaredNames(b)) innerDecl.add(n);
        // параметры вынесенных функций тоже локальны
        const pm = new RegExp('\\bfunction\\s+\\w+\\s*\\(([^)]*)\\)');
        const hit = pm.exec(b);
        if (hit) {
            hit[1].split(',').forEach(function (p) {
                const id = (p.trim().match(/^[A-Za-z_$][\w$]*/) || [])[0];
                if (id) innerDecl.add(id);
            });
        }
    }
    // Собираем ВСЕ идентификаторы, а не только «имя перед ( или .».
    //
    // Урок R644: первая версия смотрела на `имя(` и `имя.`, поэтому пропустила
    // `for (var k in OLLAMA_PARAMS)` — таблица стоит перед `)`. Ошибка ровно того же
    // класса, что и та, ради которой скрипт написан (R643: OLLAMA_PARAMS not defined).
    const KEYWORDS = new Set(['if', 'for', 'while', 'switch', 'catch', 'return', 'typeof',
        'function', 'var', 'let', 'const', 'new', 'delete', 'in', 'of', 'instanceof',
        'true', 'false', 'null', 'undefined', 'this', 'window', 'document', 'Math', 'JSON',
        'String', 'Number', 'Boolean', 'Object', 'Array', 'Date', 'parseInt', 'parseFloat',
        'isNaN', 'isFinite', 'encodeURIComponent', 'decodeURIComponent', 'console']);
    const deps = new Set();
    for (const m of text.matchAll(/(?<![.\w$])([A-Za-z_$][\w$]*)/g)) {
        const id = m[1];
        if (KEYWORDS.has(id)) continue;
        if (innerDecl.has(id)) continue;
        if (!all.has(id)) continue;          // не объявлено в файле → глобал/браузер
        deps.add(id);
    }
    return Array.from(deps).sort();
}

// --- CLI ------------------------------------------------------------------

function main(argv) {
    const json = argv.includes('--json');
    const args = argv.filter((a) => !a.startsWith('--'));
    if (args.length < 2) {
        console.error('ЗАПУСК: node scripts/webui_extract_deps.js <файл.js> <имя1> [имя2 ...] [--json]');
        return 2;
    }
    const file = args[0];
    const src = fs.readFileSync(path.resolve(file), 'utf8');
    let deps;
    try {
        deps = depsOf(src, args.slice(1));
    } catch (e) {
        console.error(String(e.message || e));
        return 1;
    }
    if (json) {
        console.log(JSON.stringify({ file: file, functions: args.slice(1), deps: deps }, null, 2));
    } else {
        console.log('файл: ' + file);
        console.log('вынесено функций: ' + args.slice(1).length);
        console.log('нужно передать извне: ' + (deps.length ? deps.join(', ') : '(ничего)'));
    }
    return 0;
}

module.exports = { declaredNames, bodyOf, depsOf };

if (require.main === module) process.exit(main(process.argv.slice(2)));
