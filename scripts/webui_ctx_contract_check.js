#!/usr/bin/env node
/**
 * webui_ctx_contract_check.js — R675 (2026-11-21): КОНТРАКТ ФАБРИК WebUI.
 *
 * ЗАЧЕМ. За R668/R669/R671/R671b/R672/R674 набрался ОДИН И ТОТ ЖЕ класс дефектов:
 * модуль выносят в фабрику `window.X = function (ctx) {…}`, а список зависимостей
 * (объект-литерал у вызывающего) с телом не сверяют. У класса две стороны, и обе
 * выглядят в браузере как «ничего не произошло»:
 *
 *   (1) ИМЯ ПОТЕРЯЛОСЬ. Тело фабрики зовёт `stripGGUF(...)`, а в ctx его не
 *       передали и в файле его нет → `ReferenceError` в обработчике клика.
 *       Оператор видит «таб не прожимается» (R672), «кнопка не работает».
 *   (2) КЛЮЧ ИНЕРТЕН. Вызывающий передаёт `foo: foo`, а фабрика `ctx.foo` не
 *       читает — зависимость «передали», но её никто не видит; при следующем
 *       выносе её забывают совсем (R672) — и дефект (1) возвращается.
 *   (3) ЧИТАЕТСЯ, НО НЕ ПЕРЕДАНО. Фабрика читает `ctx.badge`, вызывающий `badge`
 *       не передаёт → `undefined` вместо функции: дашборд падает не
 *       `ReferenceError`-ом, а `TypeError` в глубине рендера (R671b).
 *
 * Правило проверки (по одной фабрике `window.X = function (ctx) {…}`):
 *   (а) каждое СВОБОДНОЕ имя тела объявлено в файле ЛИБО связано из ctx
 *       (`var y = ctx.y`) ЛИБО является стандартным глобальным — иначе имя
 *       теряется и в рантайме будет ReferenceError;
 *   (б) каждый ключ, который передаёт вызывающий литерал `window.X({…})`,
 *       читается фабрикой (`ctx.key`) — иначе ключ инертен;
 *   (в) каждый НЕОПЦИОНАЛЬНЫЙ ключ, который фабрика читает, передаётся хотя бы
 *       одним вызывающим — иначе зависимость `undefined`. «Опциональным» считается
 *       чтение, защищённое ленивым fallback'ом (`ctx.k || …`, `ctx.k ?? …`).
 *
 * ПОЧЕМУ СВОЙ ТОКЕНАЙЗЕР, А НЕ РЕГЕКС. Наивный греп по `ctx\.(\w+)` даёт ложные
 * срабатывания ровно там, где код правильный: комментарии («ctx.badge» в
 * объяснении дефекта), строки в разметке, `var ctx = m.contextLength ? …` —
 * ЛОКАЛЬНАЯ переменная «контекст модели» в renderers-cluster.js:626,652, которая
 * ЗАТЕНЯЕТ параметр фабрики. Такой греп считает теневую переменную контрактом.
 * Поэтому здесь маленький токенайзер (строки/шаблоны/регэкспы/комментарии — не
 * код) и учёт областей видимости для затенения `ctx`.
 *
 * ИЗВЕСТНЫЕ ХРУПКИЕ МЕСТА (в списке исключений ниже, с проверкой формы). Их
 * нельзя «просто простить»: каждое исключение проверяется по тексту файла и
 * падает, если конструкция исчезла или затенений стало больше. Иначе список
 * исключений превращается в свалку, которая разрешает всё.
 *
 * ГРАНИЦЫ ПРОВЕРКИ (осознанные, чтобы правило не врало):
 *   * анализируются только фабрики с первым параметром, названным `ctx` (это и
 *     есть контракт; `window.fetch = function (input, init)` и подобные — не он);
 *   * тесты (`*.test.js`) не участвуют: они вызывают фабрики с урезанным ctx;
 *   * имена, объявленные ГДЕ УГОДНО в файле, считаются объявленными: проверка
 *     ошибается в безопасную сторону (как R574/R642 в i18n-измерителе) — иначе
 *     она объявляла бы потерянными имена, видимые через замыкание.
 *
 * ЗАПУСК:
 *   node scripts/webui_ctx_contract_check.js            # боевой код репозитория
 *   node scripts/webui_ctx_contract_check.js --root DIR # другая статика (для теста)
 *   node scripts/webui_ctx_contract_check.js --list     # что нашлось и контракт
 *
 * Код возврата: 0 — контракты сходятся, 1 — есть нарушения.
 */
'use strict';

const fs = require('fs');
const path = require('path');

const ROOT = path.join(__dirname, '..');

// Ограничение на размер файла: статика WebUI — мегабайты, но не сотни. Защита от
// случайного `--root` в корень диска (обход читает всё рекурсивно).
const MAX_FILES = 4000;

const KEYWORDS = new Set([
    'break', 'case', 'catch', 'class', 'const', 'continue', 'debugger', 'default',
    'delete', 'do', 'else', 'export', 'extends', 'finally', 'for', 'function', 'if',
    'import', 'in', 'instanceof', 'let', 'new', 'return', 'super', 'switch', 'this',
    'throw', 'try', 'typeof', 'var', 'void', 'while', 'with', 'yield', 'async',
    'await', 'of', 'get', 'set', 'static', 'true', 'false', 'null',
]);

// Стандартные глобальные имена: не «потерялись», а принадлежат платформе.
// Список намеренно короткий — только то, что реально встречается в статике WebUI
// (плюс DOM/браузер, потому что код исполняется в браузере). Добавлять сюда имя
// можно ТОЛЬКО если оно действительно глобальное: это ослабляет правило (а).
const GLOBALS = new Set([
    'undefined', 'NaN', 'Infinity', 'globalThis',
    'window', 'document', 'navigator', 'location', 'history', 'screen',
    'localStorage', 'sessionStorage', 'console', 'alert', 'confirm', 'prompt',
    'setTimeout', 'clearTimeout', 'setInterval', 'clearInterval',
    'requestAnimationFrame', 'cancelAnimationFrame', 'queueMicrotask',
    'fetch', 'XMLHttpRequest', 'WebSocket', 'EventSource', 'AbortController',
    'URL', 'URLSearchParams', 'Blob', 'File', 'FileReader', 'FormData', 'Headers',
    'Request', 'Response', 'TextEncoder', 'TextDecoder', 'CustomEvent', 'Event',
    'HTMLElement', 'Node', 'Element', 'MutationObserver', 'IntersectionObserver',
    'ResizeObserver', 'BroadcastChannel', 'Notification', 'Worker',
    'JSON', 'Math', 'Date', 'RegExp', 'Promise', 'Map', 'Set', 'WeakMap', 'WeakSet',
    'Symbol', 'Proxy', 'Reflect', 'Intl', 'Array', 'Object', 'String', 'Number',
    'Boolean', 'Function', 'Error', 'TypeError', 'RangeError', 'SyntaxError',
    'parseInt', 'parseFloat', 'isNaN', 'isFinite', 'encodeURIComponent',
    'decodeURIComponent', 'encodeURI', 'decodeURI', 'btoa', 'atob',
    'structuredClone', 'performance', 'crypto', 'arguments', 'eval',
    'require', 'module', 'exports', 'process', 'Buffer',
]);

// Регулярка может стоять после этих «точечных» токенов — иначе `/` это деление.
const REGEX_PREV_PUNCT = new Set([
    '(', ',', '=', ':', '[', '!', '&', '|', '?', '{', '}', ';', '+', '-', '*',
    '%', '<', '>', '~', '^', '`',
]);
const REGEX_PREV_WORDS = new Set([
    'return', 'typeof', 'instanceof', 'in', 'of', 'new', 'delete', 'void', 'case',
    'do', 'else', 'yield', 'await',
]);

// ---------------------------------------------------------------------------
// ТОКЕНАЙЗЕР
// ---------------------------------------------------------------------------

function isIdStart(c) {
    return /[A-Za-z_$]/.test(c);
}
function isIdPart(c) {
    return /[A-Za-z0-9_$]/.test(c);
}
function isDigit(c) {
    return c >= '0' && c <= '9';
}

/**
 * Токены: name | num | str | regex | punct | tmplsubst | tmplsubstend.
 * КОММЕНТАРИИ В ПОТОК НЕ ПОПАДАЮТ: иначе слова «ctx.badge» из объяснения дефекта,
 * `var ctx = …` из примера в комментарии и ключи вида `size:` после комментария
 * считались бы кодом. Это не косметика: именно на этом наивные проверки врут.
 */
function tokenize(src) {
    const toks = [];
    const n = src.length;
    let i = 0;
    let line = 1;
    let prev = null;
    let inTmplText = false;
    const tmplStack = []; // {depth} для ${ … } внутри шаблонной строки

    function emit(type, value) {
        const t = { type, value, line };
        toks.push(t);
        if (type !== 'comment') prev = t;
        return t;
    }

    function regexAllowed() {
        if (!prev) return true;
        if (prev.type === 'punct') return REGEX_PREV_PUNCT.has(prev.value);
        if (prev.type === 'name') return REGEX_PREV_WORDS.has(prev.value);
        return false;
    }

    while (i < n) {
        const c = src[i];
        if (c === '\n') { line++; i++; continue; }
        if (c === '\r') { i++; continue; }

        if (inTmplText) {
            if (c === '\\') { i += 2; continue; }
            if (c === '`') { inTmplText = false; i++; emit('punct', '`'); continue; }
            if (c === '$' && src[i + 1] === '{') {
                i += 2;
                tmplStack.push({ depth: 0 });
                inTmplText = false;
                emit('tmplsubst', '${');
                continue;
            }
            i++;
            continue;
        }

        if (c === ' ' || c === '\t') { i++; continue; }

        if (c === '/' && src[i + 1] === '/') {
            const j = src.indexOf('\n', i);
            i = j === -1 ? n : j;
            continue;
        }
        if (c === '/' && src[i + 1] === '*') {
            const j = src.indexOf('*/', i + 2);
            const end = j === -1 ? n : j + 2;
            for (let k = i; k < end; k++) if (src[k] === '\n') line++;
            i = end;
            continue;
        }

        if (c === '/') {
            if (!regexAllowed()) { emit('punct', '/'); i++; continue; }
            let j = i + 1;
            let inClass = false;
            while (j < n) {
                const d = src[j];
                if (d === '\\') { j += 2; continue; }
                if (d === '[') inClass = true;
                else if (d === ']') inClass = false;
                else if (d === '/' && !inClass) break;
                else if (d === '\n') break;
                j++;
            }
            j++; // закрывающий '/'
            while (j < n && /[a-z]/i.test(src[j])) j++; // флаги
            emit('regex', src.slice(i, j));
            i = j;
            continue;
        }

        if (c === '"' || c === "'") {
            let j = i + 1;
            while (j < n) {
                if (src[j] === '\\') { j += 2; continue; }
                if (src[j] === c) { j++; break; }
                j++;
            }
            emit('str', src.slice(i, j));
            i = j;
            continue;
        }

        if (c === '`') {
            inTmplText = true;
            i++;
            emit('punct', '`');
            continue;
        }

        if (isDigit(c) || (c === '.' && isDigit(src[i + 1]))) {
            let j = i;
            while (j < n && /[0-9a-fA-FxXoObBeE._+-]/.test(src[j])) {
                // '+'/'-' допустимы только в экспоненте: 1e-3
                if ((src[j] === '+' || src[j] === '-') && !/[eE]/.test(src[j - 1])) break;
                j++;
            }
            emit('num', src.slice(i, j));
            i = j;
            continue;
        }

        if (isIdStart(c)) {
            let j = i;
            while (j < n && isIdPart(src[j])) j++;
            emit('name', src.slice(i, j));
            i = j;
            continue;
        }

        // Закрывающая скобка подстановки шаблона возвращает нас в текст шаблона.
        if (c === '}' && tmplStack.length) {
            const top = tmplStack[tmplStack.length - 1];
            if (top.depth === 0) {
                tmplStack.pop();
                inTmplText = true;
                i++;
                emit('tmplsubstend', '}');
                continue;
            }
            top.depth--;
            emit('punct', '}');
            i++;
            continue;
        }
        if (c === '{' && tmplStack.length) tmplStack[tmplStack.length - 1].depth++;

        emit('punct', c);
        i++;
    }

    return toks;
}

// ---------------------------------------------------------------------------
// СТРУКТУРА: пары скобок, границы тел функций
// ---------------------------------------------------------------------------

const OPENERS = new Set(['{', '(', '[']);
const CLOSERS = new Set(['}', ')', ']']);

function matchPairs(toks) {
    const pairs = new Map();
    const stack = [];
    for (let i = 0; i < toks.length; i++) {
        const t = toks[i];
        if (t.type !== 'punct') continue;
        if (OPENERS.has(t.value)) stack.push(i);
        else if (CLOSERS.has(t.value)) {
            const open = stack.pop();
            if (open !== undefined) {
                pairs.set(open, i);
                pairs.set(i, open);
            }
        }
    }
    return pairs;
}

function sig(toks, i) {
    return toks[i] || null;
}

/** Индексы открывающих `{` тел функций (обычных и стрелочных). */
function functionBodyOpens(toks, pairs) {
    const out = new Set();
    for (let i = 0; i < toks.length; i++) {
        const t = toks[i];
        if (t.type !== 'name') continue;
        if (t.value === 'function') {
            let j = i + 1;
            if (sig(toks, j) && toks[j].type === 'name') j++;
            const p = sig(toks, j);
            if (!p || p.type !== 'punct' || p.value !== '(') continue;
            const close = pairs.get(j);
            if (close === undefined) continue;
            const b = sig(toks, close + 1);
            if (b && b.type === 'punct' && b.value === '{') out.add(close + 1);
            continue;
        }
    }
    // стрелочные: `=> {`
    for (let i = 0; i + 1 < toks.length; i++) {
        const a = toks[i];
        const b = toks[i + 1];
        if (a.type === 'punct' && a.value === '=' && b.type === 'punct' && b.value === '>') {
            const body = sig(toks, i + 2);
            if (body && body.type === 'punct' && body.value === '{') out.add(i + 2);
        }
    }
    return out;
}

// ---------------------------------------------------------------------------
// ОБЪЯВЛЕНИЯ ИМЁН В ФАЙЛЕ
// ---------------------------------------------------------------------------

function skipInitializer(toks, start) {
    let depth = 0;
    for (let i = start; i < toks.length; i++) {
        const t = toks[i];
        if (t.type === 'punct') {
            const v = t.value;
            if (OPENERS.has(v)) depth++;
            else if (CLOSERS.has(v)) {
                if (depth === 0) return { next: i, comma: false };
                depth--;
            } else if (depth === 0 && v === ',') return { next: i, comma: true };
            else if (depth === 0 && v === ';') return { next: i, comma: false };
        }
    }
    return { next: toks.length, comma: false };
}

/** Имена из шаблона привязки (var/let/const/catch/for/параметры). */
function patternNames(toks, start) {
    const out = [];
    const t = toks[start];
    if (!t) return { names: out, next: start };
    if (t.type === 'name') {
        out.push(t.value);
        return { names: out, next: start + 1 };
    }
    if (t.type === 'punct' && (t.value === '{' || t.value === '[')) {
        const close = t.value === '{' ? '}' : ']';
        let depth = 0;
        let i = start;
        for (; i < toks.length; i++) {
            const u = toks[i];
            if (u.type === 'punct' && u.value === t.value) { depth++; continue; }
            if (u.type === 'punct' && u.value === close) {
                depth--;
                if (depth === 0) { i++; break; }
                continue;
            }
            if (depth >= 1 && u.type === 'name' && !KEYWORDS.has(u.value)) {
                // В `{ a: b }` берём ОБА имени: a (свойство) и b (привязка).
                // Переоценка здесь безопасна: она не даёт правилу (а) объявить
                // потерянным имя, которое реально привязано.
                out.push(u.value);
            }
        }
        return { names: out, next: i };
    }
    return { names: out, next: start + 1 };
}

function collectDeclared(toks, pairs) {
    const out = new Set();
    for (let i = 0; i < toks.length; i++) {
        const t = toks[i];
        if (t.type !== 'name') continue;

        if (t.value === 'var' || t.value === 'let' || t.value === 'const') {
            let j = i + 1;
            for (;;) {
                const pat = patternNames(toks, j);
                for (const nm of pat.names) out.add(nm);
                j = pat.next;
                // `var a = 1, b = 2`: имя уже собрано, инициализатор пропускаем.
                const skip = skipInitializer(toks, j);
                j = skip.next;
                if (skip.comma) { j++; continue; }
                break;
            }
            continue;
        }

        if (t.value === 'function') {
            let j = i + 1;
            if (sig(toks, j) && toks[j].type === 'name' && !KEYWORDS.has(toks[j].value)) {
                out.add(toks[j].value);
                j++;
            }
            const p = sig(toks, j);
            if (p && p.type === 'punct' && p.value === '(') {
                const close = pairs.get(j);
                if (close !== undefined) {
                    for (let k = j + 1; k < close; k++) {
                        const u = toks[k];
                        if (u.type === 'name' && !KEYWORDS.has(u.value)) {
                            // Имя после '.' — свойство, не привязка (крайне редко
                            // в списке параметров, но пусть).
                            const before = sig(toks, k - 1);
                            if (before && before.type === 'punct' && before.value === '.') continue;
                            out.add(u.value);
                        }
                    }
                }
            }
            continue;
        }

        if (t.value === 'catch') {
            const p = sig(toks, i + 1);
            if (p && p.type === 'punct' && p.value === '(') {
                const close = pairs.get(i + 1);
                if (close !== undefined) {
                    for (let k = i + 2; k < close; k++) {
                        if (toks[k].type === 'name' && !KEYWORDS.has(toks[k].value)) out.add(toks[k].value);
                    }
                }
            }
            continue;
        }
    }

    // Параметры стрелочных функций: `(a, b) => …` и `x => …`.
    for (let i = 0; i + 1 < toks.length; i++) {
        const a = toks[i];
        if (!(a.type === 'punct' && a.value === '=')) continue;
        const b = toks[i + 1];
        if (!(b.type === 'punct' && b.value === '>')) continue;
        const before = toks[i - 1];
        if (!before) continue;
        if (before.type === 'name' && !KEYWORDS.has(before.value)) {
            out.add(before.value);
        } else if (before.type === 'punct' && before.value === ')') {
            const open = pairs.get(i - 1);
            if (open === undefined) continue;
            for (let k = open + 1; k < i - 1; k++) {
                if (toks[k].type === 'name' && !KEYWORDS.has(toks[k].value)) out.add(toks[k].value);
            }
        }
    }

    return out;
}

// ---------------------------------------------------------------------------
// ФАБРИКИ И ВЫЗЫВАЮЩИЕ ЛИТЕРАЛЫ
// ---------------------------------------------------------------------------

const FACTORY_PARAM = 'ctx';

function findFactories(toks, pairs) {
    const out = [];
    for (let i = 0; i + 4 < toks.length; i++) {
        const t = toks[i];
        if (!(t.type === 'name' && t.value === 'window')) continue;
        const dot = toks[i + 1];
        if (!(dot.type === 'punct' && dot.value === '.')) continue;
        const nameTok = toks[i + 2];
        if (!nameTok || nameTok.type !== 'name') continue;
        const eq = toks[i + 3];
        if (!(eq.type === 'punct' && eq.value === '=')) continue;
        const fn = toks[i + 4];
        if (!(fn && fn.type === 'name' && fn.value === 'function')) continue;

        let j = i + 5;
        if (sig(toks, j) && toks[j].type === 'name') j++;
        const openParen = sig(toks, j);
        if (!(openParen && openParen.type === 'punct' && openParen.value === '(')) continue;
        const closeParen = pairs.get(j);
        if (closeParen === undefined) continue;
        const params = [];
        for (let k = j + 1; k < closeParen; k++) {
            if (toks[k].type === 'name' && !KEYWORDS.has(toks[k].value)) params.push(toks[k].value);
        }
        const openBrace = sig(toks, closeParen + 1);
        if (!(openBrace && openBrace.type === 'punct' && openBrace.value === '{')) continue;
        const closeBrace = pairs.get(closeParen + 1);
        if (closeBrace === undefined) continue;

        out.push({
            name: nameTok.value,
            param: params[0],
            params,
            bodyOpen: closeParen + 1,
            bodyClose: closeBrace,
            line: t.line,
        });
        i = closeBrace;
    }
    return out;
}

/** Ключи объект-литерала, переданного вызывающим `window.X({ … })`. */
function findCalls(toks, pairs) {
    const out = [];
    for (let i = 0; i + 3 < toks.length; i++) {
        const t = toks[i];
        if (!(t.type === 'name' && t.value === 'window')) continue;
        const dot = toks[i + 1];
        if (!(dot.type === 'punct' && dot.value === '.')) continue;
        const nameTok = toks[i + 2];
        if (!nameTok || nameTok.type !== 'name') continue;
        const openParen = toks[i + 3];
        if (!(openParen.type === 'punct' && openParen.value === '(')) continue;
        const first = sig(toks, i + 4);
        if (!(first && first.type === 'punct' && first.value === '{')) continue;
        const closeBrace = pairs.get(i + 4);
        if (closeBrace === undefined) continue;

        const keys = [];
        let depth = 0;
        for (let k = i + 5; k < closeBrace; k++) {
            const u = toks[k];
            if (u.type === 'punct') {
                if (OPENERS.has(u.value)) depth++;
                else if (CLOSERS.has(u.value)) depth--;
                continue;
            }
            if (depth !== 0) continue;
            if (u.type !== 'name' && u.type !== 'str') continue;
            const next = sig(toks, k + 1);
            if (!next) continue;
            const isKey = (next.type === 'punct' && next.value === ':');
            const isShorthand = (next.type === 'punct' && (next.value === ',' || next.value === '}'));
            if (!isKey && !isShorthand) continue;
            let key = u.value;
            if (u.type === 'str') key = key.slice(1, -1);
            if (KEYWORDS.has(key)) continue;
            keys.push({ key, line: u.line });
        }
        out.push({ factory: nameTok.value, keys, line: t.line });
    }
    return out;
}

/**
 * ГЛОБАЛЬНОЕ ПРОСТРАНСТВО ИМЁН WebUI: `window.<Name> = …` в любом боевом файле.
 *
 * ЗАЧЕМ. Статика WebUI — классические скрипты (без модулей), поэтому
 * `window.Foo = …` в одном файле делает `Foo` законным bare-именем в любом другом
 * (область видимости заканчивается глобальным объектом). Именно так модули берут
 * `I18N`, `Utils`, `App`, `BackendTypeFilter`: их в ctx никто не передаёт, и это
 * НЕ ReferenceError. Без этого набора правило (а) объявляло бы потерянными живые
 * имена — то есть валило бы правильный код.
 *
 * ВАЖНО: локальные функции, вынесенные в соседний файл (`badge`, `stripGGUF`,
 * `formatFileSize`), здесь НЕ появятся — они не глобальные. Поэтому правило (а)
 * продолжает ловить именно тот класс, ради которого написано.
 */
function collectGlobalNamespace(toks) {
    const out = new Set();
    for (let i = 0; i + 3 < toks.length; i++) {
        const t = toks[i];
        if (!(t.type === 'name' && t.value === 'window')) continue;
        const dot = toks[i + 1];
        if (!(dot.type === 'punct' && dot.value === '.')) continue;
        const nameTok = toks[i + 2];
        if (!nameTok || nameTok.type !== 'name') continue;
        const eq = toks[i + 3];
        if (!(eq.type === 'punct' && eq.value === '=')) continue;
        out.add(nameTok.value);
    }
    return out;
}

// ---------------------------------------------------------------------------
// АНАЛИЗ ТЕЛА ФАБРИКИ
// ---------------------------------------------------------------------------

/** Ключ сразу за `ctx.` / `ctx["…"]`, либо null. */
function readKeyAt(toks, i) {
    const dot = sig(toks, i + 1);
    if (dot && dot.type === 'punct' && dot.value === '.') {
        const key = sig(toks, i + 2);
        if (key && key.type === 'name') return { key: key.value, keyIndex: i + 2 };
        return null;
    }
    if (dot && dot.type === 'punct' && dot.value === '[') {
        const key = sig(toks, i + 2);
        const close = sig(toks, i + 3);
        if (key && key.type === 'str' && close && close.type === 'punct' && close.value === ']') {
            return { key: key.value.slice(1, -1), keyIndex: i + 2 };
        }
    }
    return null;
}

function analyzeFactoryBody(toks, pairs, factory, fnBodyOpens) {
    const param = factory.param;
    const start = factory.bodyOpen;
    const end = factory.bodyClose;

    // Параметры тела функции, объявленного в теле фабрики (для затенения param).
    function paramsOf(bodyOpen) {
        const before = sig(toks, bodyOpen - 1);
        if (!before) return [];
        if (before.type === 'punct' && before.value === ')') {
            const open = pairs.get(bodyOpen - 1);
            if (open === undefined) return [];
            const names = [];
            for (let k = open + 1; k < bodyOpen - 1; k++) {
                if (toks[k].type === 'name' && !KEYWORDS.has(toks[k].value)) names.push(toks[k].value);
            }
            return names;
        }
        // `x => {` — один параметр
        if (before.type === 'punct' && before.value === '>') {
            const eq = sig(toks, bodyOpen - 2);
            if (eq && eq.type === 'punct' && eq.value === '=') {
                const p = sig(toks, bodyOpen - 3);
                if (!p) return [];
                if (p.type === 'name' && !KEYWORDS.has(p.value)) return [p.value];
                if (p.type === 'punct' && p.value === ')') {
                    const open = pairs.get(bodyOpen - 3);
                    if (open === undefined) return [];
                    const names = [];
                    for (let k = open + 1; k < bodyOpen - 4; k++) {
                        if (toks[k].type === 'name' && !KEYWORDS.has(toks[k].value)) names.push(toks[k].value);
                    }
                    return names;
                }
            }
        }
        return [];
    }

    /** Конец выражения тела стрелки без фигурных скобок (`ctx => ctx.name`). */
    function arrowExprEnd(from) {
        let depth = 0;
        for (let k = from; k < end; k++) {
            const u = toks[k];
            if (u.type !== 'punct') continue;
            if (OPENERS.has(u.value)) { depth++; continue; }
            if (u.value === '}' || u.value === ']') {
                if (depth === 0) return k - 1;
                depth--;
                continue;
            }
            if (u.value === ')') {
                if (depth === 0) return k - 1;
                depth--;
                continue;
            }
            if (u.value === ',' && depth === 0) return k - 1;
            if (u.value === ';' && depth === 0) return k - 1;
        }
        return end;
    }

    const shadows = [];
    const seen = new Set();
    function addShadow(a, b, line) {
        const key = a + ':' + b;
        if (seen.has(key)) return;
        seen.add(key);
        shadows.push({ start: a, end: b, line });
    }

    for (let i = start + 1; i < end; i++) {
        const t = toks[i];

        // (1) объявление var/let/const с именем параметра: область — тело
        // ближайшей включающей функции (для `var` это ровно так; для let/const
        // область уже, но расширение безопасно: чтения контракта не «пропадут»,
        // они просто не зачтутся внутри области затенения).
        if (t.type === 'name' && (t.value === 'var' || t.value === 'let' || t.value === 'const')) {
            const pat = patternNames(toks, i + 1);
            if (!pat.names.includes(param)) continue;
            let regionOpen = start;
            let regionClose = end;
            for (const open of fnBodyOpens) {
                if (open <= i && pairs.get(open) >= i && open > regionOpen) {
                    regionOpen = open;
                    regionClose = pairs.get(open);
                }
            }
            addShadow(regionOpen, regionClose, t.line);
            continue;
        }

        // (2) параметр вложенной функции/стрелки с тем же именем:
        //     contexts.map(ctx => `… ${ctx.name} …`) — самая частая ловушка.
        if (t.type === 'punct' && t.value === '=' ) {
            const gt = sig(toks, i + 1);
            if (!gt || gt.type !== 'punct' || gt.value !== '>') continue;
            // Параметры стрелки.
            let arrowParamNames = [];
            const p = sig(toks, i - 1);
            if (p && p.type === 'name' && !KEYWORDS.has(p.value)) arrowParamNames = [p.value];
            else if (p && p.type === 'punct' && p.value === ')') {
                const open = pairs.get(i - 1);
                if (open !== undefined) {
                    for (let k = open + 1; k < i - 1; k++) {
                        if (toks[k].type === 'name' && !KEYWORDS.has(toks[k].value)) arrowParamNames.push(toks[k].value);
                    }
                }
            }
            if (!arrowParamNames.includes(param)) continue;
            const body = sig(toks, i + 2);
            if (body && body.type === 'punct' && body.value === '{') {
                addShadow(i + 2, pairs.get(i + 2), t.line);
            } else {
                addShadow(i + 2, arrowExprEnd(i + 2), t.line);
            }
        }
    }

    // (3) `function (ctx) { … }` внутри тела фабрики.
    for (const open of fnBodyOpens) {
        if (open <= start || open >= end) continue;
        const names = paramsOf(open);
        if (names.includes(param)) addShadow(open, pairs.get(open), toks[open].line);
    }

    function shadowed(i) {
        return shadows.some((s) => i >= s.start && i <= s.end);
    }

    // 2) чтения контракта: ctx.KEY, ctx['KEY'], привязки var y = ctx.KEY.
    const reads = new Map();      // key -> [{line, guarded}]
    const bindings = new Map();   // name -> key
    for (let i = start + 1; i < end; i++) {
        const t = toks[i];
        if (!(t.type === 'name' && t.value === param)) continue;
        if (shadowed(i)) continue;
        const found = readKeyAt(toks, i);
        if (!found) continue;
        // Защита от падения при отсутствии зависимости: `ctx.k || …`, `ctx.k ?? …`,
        // `ctx.k && …`, `ctx.k ? …`.
        const after = sig(toks, found.keyIndex + 1);
        const guarded = !!(after && after.type === 'punct' &&
            (after.value === '|' || after.value === '?' || after.value === '&'));
        const list = reads.get(found.key) || [];
        list.push({ line: t.line, guarded });
        reads.set(found.key, list);

        const before = sig(toks, i - 1);
        const before2 = sig(toks, i - 2);
        if (before && before.type === 'punct' && before.value === '=' &&
            before2 && before2.type === 'name' && !KEYWORDS.has(before2.value)) {
            bindings.set(before2.value, found.key);
        }
    }

    return { reads, bindings, shadows };
}

// ---------------------------------------------------------------------------
// ИЗВЕСТНЫЕ ХРУПКИЕ МЕСТА (исключения правила, каждое проверяется)
// ---------------------------------------------------------------------------

const KNOWN_FRAGILE = [
    {
        file: 'webui/js/modules/gguf-detail-params.js',
        factory: 'GgufDetailParams',
        name: 'showToast',
        kind: 'guarded-optional',
        reason: 'ctx.showToast не передаётся вызывающим (gguf-detail-settings.js:516). ' +
            'Зависимость резолвится лениво: ctx.showToast || M.showToast || window.showToast ' +
            '(R668). Правило (в) считает чтение опциональным, но список фиксирует случай: ' +
            'если обёртка исчезнет, тост перестанет работать молча — тогда исключение устарело.',
        verify: [/var\s+showToast\s*=\s*function\s*\(/, /ctx\.showToast/],
    },
    {
        file: 'webui/js/modules/gguf-detail-settings.js',
        factory: 'GgufDetailSettings',
        name: 'showToast',
        kind: 'guarded-optional',
        reason: 'Тот же ленивый резолвер, что и в gguf-detail-params.js (R668): ' +
            'ctx.showToast || M.showToast || window.showToast.',
        verify: [/var\s+showToast\s*=\s*function\s*\(/, /ctx\.showToast/],
    },
    {
        file: 'webui/js/modules/app-data.js',
        factory: 'AppData',
        name: '_t',
        kind: 'guarded-optional',
        reason: 'ctx._t вызывающий не передаёт (app.js:872). В файле есть локальный ' +
            'fallback `var _t = ctx._t || function (key, fallback) {…}` через I18N (R671): ' +
            'без него имя осталось бы свободным (как было до R671).',
        verify: [/var\s+_t\s*=\s*ctx\._t\s*\|\|/],
    },
    {
        file: 'webui/js/modules/renderers-cluster.js',
        factory: 'RenderersCluster',
        name: 'ctx',
        kind: 'shadowed-param',
        reason: 'Имя `ctx` в этом файле занято не только контрактом: (1) ЛОКАЛЬНАЯ ' +
            'переменная «контекст модели» в renderers-cluster.js:626,652 ' +
            '(`var ctx = m.contextLength ? …`) и (2) параметр стрелки в ' +
            'renderers-cluster.js:167 (`contexts.map(ctx => …)`) — там `ctx.name`, ' +
            '`ctx.effectiveContext` и прочие поля принадлежат элементу массива, а НЕ ' +
            'фабрике. Именно здесь наивный греп по ctx\\.(\\w+) перестаёт различать ' +
            'контракт и теневую переменную и требует 8 несуществующих ключей. ' +
            'Затенений зафиксировано ровно 3: новое — сигнал перепроверить контракт.',
        pinnedShadows: 3,
    },
];

function checkKnownFragile(readFileText, errors, notes) {
    for (const ex of KNOWN_FRAGILE) {
        const text = readFileText(ex.file);
        if (text === null) {
            errors.push(`${ex.file}: исключение для ${ex.name} ссылается на отсутствующий файл`);
            continue;
        }
        if (ex.verify) {
            for (const re of ex.verify) {
                if (!re.test(text)) {
                    errors.push(`${ex.file}: хрупкая конструкция для ${ex.name} исчезла ` +
                        `(не найдено ${re}) — ${ex.reason}`);
                }
            }
        }
        const extra = ex.pinnedShadows ? `, затенений зафиксировано ${ex.pinnedShadows}`
            : (ex.verify ? ', форма резолвера проверена' : '');
        notes.push(`${ex.file}: ${ex.name} — ${ex.kind}${extra}`);
    }
}

// ---------------------------------------------------------------------------
// ОСНОВНАЯ ПРОВЕРКА
// ---------------------------------------------------------------------------

function walkJs(dir, out) {
    if (out.length > MAX_FILES) return out;
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
        const full = path.join(dir, entry.name);
        if (entry.isDirectory()) {
            if (entry.name === 'node_modules') continue;
            walkJs(full, out);
            continue;
        }
        if (!entry.name.endsWith('.js')) continue;
        if (entry.name.endsWith('.test.js')) continue;
        out.push(full);
    }
    return out;
}

function relOf(root, file) {
    return path.relative(path.join(root, '..'), file).split(path.sep).join('/');
}

function run(options) {
    const opts = options || {};
    const staticRoot = opts.root ? path.resolve(opts.root) : path.join(ROOT, 'webui');
    const jsRoot = path.join(staticRoot, 'js');
    const errors = [];
    const notes = [];
    const infos = [];

    if (!fs.existsSync(jsRoot)) {
        return { errors: [`нет каталога со скриптами: ${jsRoot}`], notes, infos };
    }

    const files = walkJs(jsRoot, []).sort();
    const texts = new Map();
    const perFile = [];
    const globalNames = new Set();

    for (const file of files) {
        const src = fs.readFileSync(file, 'utf8');
        texts.set(file, src);
        const toks = tokenize(src);
        const pairs = matchPairs(toks);
        for (const nm of collectGlobalNamespace(toks)) globalNames.add(nm);
        const factories = findFactories(toks, pairs).filter((f) => f.param === FACTORY_PARAM);
        const calls = findCalls(toks, pairs);
        perFile.push({ file, toks, pairs, factories, calls, declared: collectDeclared(toks, pairs) });
    }

    // Вызовы собираем по имени фабрики через все файлы: одна фабрика может
    // вызываться из нескольких мест (gguf-detail-params — из settings и render).
    const callsByName = new Map();
    for (const pf of perFile) {
        for (const call of pf.calls) {
            const list = callsByName.get(call.factory) || [];
            list.push({ file: relOf(staticRoot, pf.file), ...call });
            callsByName.set(call.factory, list);
        }
    }

    const factoriesByName = new Map();
    for (const pf of perFile) {
        for (const f of pf.factories) {
            const list = factoriesByName.get(f.name) || [];
            list.push({ file: relOf(staticRoot, pf.file), fileAbs: pf.file, ...f });
            factoriesByName.set(f.name, list);
        }
    }

    const fnBodyCache = new Map();

    for (const pf of perFile) {
        const rel = relOf(staticRoot, pf.file);
        let fnBodyOpens = fnBodyCache.get(pf.file);
        if (!fnBodyOpens) {
            fnBodyOpens = functionBodyOpens(pf.toks, pf.pairs);
            fnBodyCache.set(pf.file, fnBodyOpens);
        }

        for (const factory of pf.factories) {
            const { reads, bindings, shadows } = analyzeFactoryBody(pf.toks, pf.pairs, factory, fnBodyOpens);
            const where = `${rel}:${factory.line}`;

            // (0) затенение параметра. Само по себе это не ошибка (переменная
            // «контекст модели» — обычное имя), но чтения внутри области затенения
            // контрактом НЕ считаются, поэтому случай фиксируется в отчёте. Если
            // для файла есть исключение с pinnedShadows — сверяем количество:
            // новое затенение означает, что контракт читается не тем именем.
            if (shadows.length) {
                const ex = KNOWN_FRAGILE.find((e) => e.file === rel && e.name === factory.param &&
                    e.kind === 'shadowed-param');
                if (ex && typeof ex.pinnedShadows === 'number' && shadows.length !== ex.pinnedShadows) {
                    errors.push(`${where}: затенений ${factory.param} стало ${shadows.length}, ` +
                        `зафиксировано ${ex.pinnedShadows} (строки ${shadows.map((s) => s.line).join(', ')}) — ` +
                        'перепроверьте контракт: чтение могло уйти в теневую переменную');
                } else {
                    notes.push(`${where}: ${factory.name} — ${factory.param} затенён на строке(ах) ` +
                        `${shadows.map((s) => s.line).join(', ')}; эти чтения контрактом не считаются` +
                        (ex ? ' (исключение подтверждено)' : ''));
                }
            }

            // (а) свободные имена тела.
            const calls = callsByName.get(factory.name) || [];
            const passedKeys = new Set();
            for (const c of calls) for (const k of c.keys) passedKeys.add(k.key);

            // Одно имя — одна запись, даже если упоминается десять раз: иначе
            // отчёт превращается в список повторов и его перестают читать.
            const freeHits = new Map(); // name -> {line, count}
            for (let i = factory.bodyOpen + 1; i < factory.bodyClose; i++) {
                const t = pf.toks[i];
                if (t.type !== 'name') continue;
                const name = t.value;
                if (KEYWORDS.has(name)) continue;
                if (name === factory.param) continue;
                if (GLOBALS.has(name)) continue;
                if (globalNames.has(name)) continue;
                if (pf.declared.has(name)) continue;
                if (bindings.has(name)) continue;

                const before = sig(pf.toks, i - 1);
                const after = sig(pf.toks, i + 1);

                // свойство: obj.name
                if (before && before.type === 'punct' && before.value === '.') continue;
                // ключ объект-литерала: `{ name: … }` / `{ … , name: … }`
                if (after && after.type === 'punct' && after.value === ':' && before &&
                    before.type === 'punct' && (before.value === '{' || before.value === ',' || before.value === ';')) {
                    continue;
                }
                // метод/геттер объекта: `name(…) {` — это определение, не ссылка
                if (after && after.type === 'punct' && after.value === '(') {
                    const close = pf.pairs.get(i + 1);
                    const brace = close === undefined ? null : sig(pf.toks, close + 1);
                    if (brace && brace.type === 'punct' && brace.value === '{') continue;
                }
                // typeof X — безопасная проверка существования
                if (before && before.type === 'name' && before.value === 'typeof') continue;

                const ex = KNOWN_FRAGILE.find((e) => e.file === rel && e.name === name);
                if (ex) {
                    if (!freeHits.has(name)) freeHits.set(name, { line: t.line, count: 0, excused: true });
                    freeHits.get(name).count++;
                    continue;
                }
                if (!freeHits.has(name)) freeHits.set(name, { line: t.line, count: 0, excused: false });
                freeHits.get(name).count++;
            }

            for (const [name, hit] of freeHits) {
                if (hit.excused) {
                    notes.push(`${where}: свободное имя ${name} допущено исключением (${hit.count} упоминаний)`);
                    continue;
                }
                errors.push(`${where}: фабрика ${factory.name} использует имя "${name}" ` +
                    `(${hit.count} упоминаний, первое — строка ${hit.line}), которого нет ни в файле, ` +
                    'ни в ctx, ни среди глобальных — в рантайме это ReferenceError ' +
                    '(класс R668/R669/R671/R672). Передайте его в ctx фабрики или объявите в файле');
            }

            // (б) ключи вызывающего литерала должны читаться фабрикой.
            for (const call of calls) {
                for (const k of call.keys) {
                    if (reads.has(k.key) || bindings.has(k.key)) continue;
                    errors.push(`${call.file}:${k.line}: ключ ${k.key} передан в ${factory.name}, ` +
                        `но фабрика (${where}) его не читает — ключ инертен (класс R672). ` +
                        'Либо читать ctx.' + k.key + ', либо не передавать');
                }
            }

            // (в) читаемые ключи должны передаваться хотя бы одним вызывающим.
            if (calls.length) {
                for (const [key, list] of reads) {
                    if (passedKeys.has(key)) continue;
                    const allGuarded = list.every((r) => r.guarded);
                    const ex = KNOWN_FRAGILE.find((e) => e.file === rel && e.name === key && e.kind === 'guarded-optional');
                    if (allGuarded || ex) {
                        notes.push(`${where}: ctx.${key} не передаётся, но читается лениво ` +
                            `(строки ${list.map((r) => r.line).join(', ')})` + (ex ? ' — исключение подтверждено' : ''));
                        continue;
                    }
                    errors.push(`${where}: фабрика ${factory.name} читает ctx.${key} ` +
                        `(строки ${list.map((r) => r.line).join(', ')}), но ни один вызывающий его ` +
                        'не передаёт — зависимость будет undefined (класс R671b)');
                }
            }

            infos.push(`${where}: ${factory.name} — чтений ${reads.size}, привязок ${bindings.size}, ` +
                `вызовов ${calls.length}` + (shadows.length ? `, затенений ${shadows.length}` : ''));
        }
    }

    checkKnownFragile((rel) => {
        const abs = path.resolve(staticRoot, '..', rel);
        if (fs.existsSync(abs)) return fs.readFileSync(abs, 'utf8');
        // Файл может лежать в другой статике при --root: ищем по имени.
        const base = path.basename(rel);
        for (const [file] of texts) if (path.basename(file) === base) return texts.get(file);
        return null;
    }, errors, notes);

    return { errors, notes, infos };
}

function main() {
    const argv = process.argv.slice(2);
    // Поддерживаются обе формы (`--root=DIR` и `--root DIR`): первая короче для
    // CI, вторая — обычная для ручного запуска. Молча проигнорированный
    // разделитель превратил бы «проверку другой статики» в проверку репозитория
    // (найдено при отладке: `--root <dir>` давал зелёный на любом аргументе).
    const eqArg = argv.find((a) => a.startsWith('--root='));
    const rootFlag = argv.indexOf('--root');
    let root = null;
    if (eqArg) root = eqArg.slice('--root='.length);
    else if (rootFlag !== -1 && argv[rootFlag + 1]) root = argv[rootFlag + 1];
    const options = root ? { root } : {};

    const { errors, notes, infos } = run(options);

    if (argv.includes('--list')) {
        for (const line of infos) console.log('  ' + line);
        console.log('');
    }

    console.log(`[ctx-contract] фабрик window.X = function (ctx) проверено: ${infos.length}`);
    for (const n of notes) console.log('  · ' + n);

    if (errors.length) {
        console.error(`\n[ctx-contract] FAIL — нарушений контракта: ${errors.length}`);
        for (const e of errors) console.error('  * ' + e);
        return 1;
    }

    console.log('[ctx-contract] OK: свободных имён без объявления нет, ' +
        'ключи вызывающих читаются, читаемые ключи передаются');
    return 0;
}

if (require.main === module) {
    process.exit(main());
}

module.exports = { run, tokenize, collectDeclared, KNOWN_FRAGILE };
