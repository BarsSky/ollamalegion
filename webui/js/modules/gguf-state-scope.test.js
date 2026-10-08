// gguf-state-scope.test.js — R91 (2026-10-08): статическая защита от голого
// `state` в модулях страницы GGUF.
//
// Запуск: node webui/js/modules/gguf-state-scope.test.js
//
// ЖИВОЙ ДЕФЕКТ, КОТОРЫЙ ЭТО ЛОВИТ. В `gguf-renderer-refresh.js` функция
// `fetchWorkerVersionAsync` обращалась к `state.workerVersions` без объявления
// (в R65d module-scope `const state = M.state;` убрали, а функцию добавили позже
// в старом стиле). В браузере глобального `state` нет → `ReferenceError: state is
// not defined` при КАЖДОМ обновлении деталей. Вызов стоит в refreshDetail ПОСЛЕ
// заполнения workerInfo/gpuInfo, поэтому исключение обрывало остаток обработчика:
// не заполнялись localModels/loadedModels/backendRuntime и не вызывалась
// перерисовка. Снаружи это выглядело как «пропала вся информация кроме инфо —
// какие модели, какие настройки».
//
// ПОЧЕМУ СТАТИЧЕСКИЙ ТЕСТ, А НЕ ПРОСТО ЮНИТ. Ошибка не в логике, а в области
// видимости: любой новый вызов в этом стиле снова даст пустые вкладки, причём
// молча (исключение ловит .catch и пишет только в консоль). Проверка исходников
// стоит миллисекунды и не требует DOM.
//
// ПРАВИЛО. Если функция использует `state.`/`state[`, то `state` должен быть:
//   * объявлен в самой функции (`const state = M.state;`), либо
//   * её параметром, либо
//   * объявлен на уровне модуля (тогда годится для всех функций модуля).

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const MODULES_DIR = __dirname;

// Какие файлы проверяем: модули рендерера GGUF (без тестов).
const TARGETS = fs.readdirSync(MODULES_DIR)
    .filter(function (f) { return /^gguf-renderer.*\.js$/.test(f) && !/\.test\.js$/.test(f); })
    .sort();

assert.ok(TARGETS.length > 0, 'не найдено ни одного gguf-renderer*.js — тест устарел');

// Использование переменной state (не `M.state`, не `x.state`, не `stateful`).
const USE_RE = /(^|[^.\w$])state\s*[.[]/m;
// Объявление внутри блока.
const DECL_RE = /(const|let|var)\s+state\s*=/;
// Объявление на уровне модуля: отступ в 4 пробела (внутри IIFE), вне функции.
const MODULE_DECL_RE = /^\s{4}(const|let|var)\s+state\s*=\s*M\.state\s*;/m;

/**
 * stripComments — убрать // и /* *\/ комментарии.
 *
 * БЕЗ ЭТОГО ПРОВЕРКА БЕСПОЛЕЗНА, и это выяснилось на живом примере: в
 * комментарии-объяснении к правке стоит текст `const state = M.state;`, и
 * детектор считал объявление существующим, хотя в коде его не было. Комментарии
 * для анализа области видимости — не код.
 */
function stripComments(src) {
    let out = '';
    let i = 0;
    let inLine = false;
    let inBlock = false;
    let quote = null;
    while (i < src.length) {
        const c = src[i];
        const n = src[i + 1];
        if (inLine) {
            if (c === '\n') { inLine = false; out += c; }
            i++;
            continue;
        }
        if (inBlock) {
            if (c === '*' && n === '/') { inBlock = false; i += 2; continue; }
            if (c === '\n') out += c; // сохраняем нумерацию строк
            i++;
            continue;
        }
        if (quote) {
            out += c;
            if (c === '\\') { out += n || ''; i += 2; continue; }
            if (c === quote) quote = null;
            i++;
            continue;
        }
        if (c === '"' || c === "'" || c === '`') { quote = c; out += c; i++; continue; }
        if (c === '/' && n === '/') { inLine = true; i += 2; continue; }
        if (c === '/' && n === '*') { inBlock = true; i += 2; continue; }
        out += c;
        i++;
    }
    return out;
}

/**
 * extractFunctions — вытащить тела функций с балансом скобок.
 *
 * ПОЧЕМУ БАЛАНС СКОБОК, А НЕ «ДО СЛЕДУЮЩЕГО function». Первая версия проверки
 * резала текст по строкам `    function `, и тело `fetchWorkerVersionAsync`
 * «съедало» следующие за ней `M.refreshBackends = function() {…}` и
 * `M.refreshDetail = function() {…}` — в них есть свои `const state = M.state;`,
 * поэтому проверка считала объявление найденным и НЕ ЛОВИЛА живой дефект
 * (проверено на реальном файле с удалённой правкой). Скобочный баланс даёт точные
 * границы тела.
 *
 * Поддерживаются обе формы объявления: `function name(...) {` и
 * `M.name = function(...) {`.
 */
function extractFunctions(src) {
    const out = [];
    const re = /^ {4}(?:function\s+([A-Za-z0-9_$]+)|M\.([A-Za-z0-9_$]+)\s*=\s*function)\s*(\([^)]*\))\s*\{/gm;
    let m;
    while ((m = re.exec(src)) !== null) {
        const start = m.index + m[0].length;
        let depth = 1;
        let i = start;
        let quote = null;
        while (i < src.length && depth > 0) {
            const c = src[i];
            if (quote) {
                if (c === '\\') { i += 2; continue; }
                if (c === quote) quote = null;
                i++;
                continue;
            }
            if (c === '"' || c === "'" || c === '`') { quote = c; i++; continue; }
            if (c === '{') depth++;
            else if (c === '}') depth--;
            i++;
        }
        out.push({ name: m[1] || m[2], header: m[0], body: src.slice(start, i - 1) });
        re.lastIndex = i;
    }
    return out;
}

/**
 * checkSource — вернуть список «функция использует state, но не объявляет его» для
 * одного исходника.
 */
function checkSource(rawSrc, label) {
    const problems = [];
    const src = stripComments(rawSrc);
    if (MODULE_DECL_RE.test(src)) return problems; // алиас модуля покрывает все функции

    extractFunctions(src).forEach(function (fn) {
        const usesState = USE_RE.test(fn.body);
        const declares = DECL_RE.test(fn.body);
        const isParam = /\([^)]*\bstate\b[^)]*\)/.test(fn.header);
        if (usesState && !declares && !isParam) {
            problems.push(label + ': ' + fn.name + '() использует state без объявления');
        }
    });
    return problems;
}

let checks = 0;
function check(name, fn) {
    fn();
    checks++;
    console.log('  ✓ ' + name);
}

// --- Самопроверка: детектор обязан ловить именно этот класс ошибки -----------
check('детектор ловит голый state внутри функции', function () {
    const bad = [
        '(function () {',
        "    const M = window.GgufModule;",
        '    function fetchWorkerVersionAsync(backendId) {',
        '        if (!backendId) return;',
        '        const cached = state.workerVersions && state.workerVersions[backendId];',
        '        return cached;',
        '    }',
        '    function neighbour() {',
        '        const state = M.state;',
        '        return state.detailPane;',
        '    }',
        '})();'
    ].join('\n');
    const found = checkSource(bad, 'synthetic');
    assert.strictEqual(found.length, 1,
        'детектор не нашёл голый state (или зацепил соседнюю функцию): ' + JSON.stringify(found));
    assert.ok(/fetchWorkerVersionAsync/.test(found[0]), 'не названа виновная функция: ' + found[0]);
});

check('детектор НЕ ругается на корректные варианты', function () {
    const ok = [
        '(function () {',
        "    const M = window.GgufModule;",
        '    function a() {',
        '        const state = M.state;',
        '        return state.detailPane;',
        '    }',
        '    function b(backendId, state) {',
        '        return state.localModels;',
        '    }',
        '    function c() {',
        '        return M.state.detailPane;',
        '    }',
        '})();'
    ].join('\n');
    assert.deepStrictEqual(checkSource(ok, 'synthetic-ok'), []);
});

check('детектор принимает module-scope алиас', function () {
    const ok = [
        '(function () {',
        "    const M = window.GgufModule;",
        '    const state = M.state;',
        '    function a() { return state.detailPane; }',
        '})();'
    ].join('\n');
    assert.deepStrictEqual(checkSource(ok, 'synthetic-alias'), []);
});

check('упоминание объявления в КОММЕНТАРИИ не считается объявлением', function () {
    // Живой случай: рядом с правкой стоит комментарий «…был module-scope
    // `const state = M.state;`…», и без вырезания комментариев детектор считал
    // объявление существующим — то есть пропускал ровно тот дефект, ради
    // которого написан.
    const bad = [
        '(function () {',
        "    const M = window.GgufModule;",
        '    function fetchWorkerVersionAsync(backendId) {',
        '        // История: раньше здесь был module-scope `const state = M.state;`',
        '        if (!backendId) return;',
        '        return state.workerVersions[backendId];',
        '    }',
        '})();'
    ].join('\n');
    const found = checkSource(bad, 'synthetic-comment');
    assert.strictEqual(found.length, 1,
        'детектор поверил комментарию вместо кода: ' + JSON.stringify(found));
});

// --- Реальные модули страницы GGUF ------------------------------------------
check('все gguf-renderer*.js: state всегда объявлен там, где используется', function () {
    const problems = [];
    TARGETS.forEach(function (name) {
        const src = fs.readFileSync(path.join(MODULES_DIR, name), 'utf8');
        problems.push.apply(problems, checkSource(src, name));
    });
    assert.deepStrictEqual(problems, [],
        'голый `state` даст ReferenceError и пустые вкладки в браузере:\n  ' + problems.join('\n  '));
});

console.log('\nOK: ' + checks + ' checks passed (' + TARGETS.length + ' модулей проверено)');
