#!/usr/bin/env node
/*
 * check_webui_refs.js — статическая проверка ссылок в webui/js.
 *
 * Зачем: модули WebUI — это classic-scripts с общими IIFE, поэтому вызов
 * функции, которой нет ни в одном файле (или которая объявлена внутри чужого
 * IIFE), падает только в рантайме. Именно так в webui проскочили
 * `closeModal is not defined` (app-modals.js публикует window.BackendCRUD.*)
 * и `autoSaveSettings is not defined` вне IIFE.
 *
 * Что делает: собирает все объявленные имена (function/var/let/const/window.X/
 * объектные ключи верхнего уровня) по всем js-файлам webui/js + index.html-скриптам
 * и сообщает о вызовах вида `name(`, для которых ни одного объявления не нашлось.
 *
 * Запуск: node scripts/check_webui_refs.js [webui/js]
 */

const fs = require('fs');
const path = require('path');

const root = process.argv[2] || path.join(__dirname, '..', 'webui', 'js');

function walk(dir, out = []) {
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) walk(p, out);
    else if (e.name.endsWith('.js')) out.push(p);
  }
  return out;
}

const files = walk(root);
const declared = new Set();
const texts = new Map();

// Глобальные browser/JS API, которые не нужно объявлять.
const BUILTINS = new Set([
  'if','for','while','switch','catch','return','typeof','function','new','await','async','do','else','delete','void','in','of','case','try','finally','throw','yield','super','this','class','const','let','var','import','export','default','extends','instanceof','null','true','false','undefined','NaN','Infinity',
  'parseInt','parseFloat','isNaN','isFinite','String','Number','Boolean','Object','Array','JSON','Math','Date','RegExp','Error','TypeError','Map','Set','WeakMap','WeakSet','Promise','Symbol','Proxy','Reflect','BigInt','decodeURIComponent','encodeURIComponent','decodeURI','encodeURI','setTimeout','clearTimeout','setInterval','clearInterval','requestAnimationFrame','cancelAnimationFrame','fetch','alert','confirm','prompt','btoa','atob','structuredClone','queueMicrotask','console','document','window','localStorage','sessionStorage','location','navigator','history','performance','CustomEvent','Event','EventSource','WebSocket','BroadcastChannel','Blob','File','FileReader','FormData','Headers','Request','Response','URL','URLSearchParams','XMLHttpRequest','AbortController','IntersectionObserver','MutationObserver','ResizeObserver','getComputedStyle','matchMedia','crypto','TextEncoder','TextDecoder','Intl','Image','Audio','Chart','getElementById','querySelector','querySelectorAll'
]);

for (const f of files) {
  const src = fs.readFileSync(f, 'utf8');
  texts.set(f, src);
  // function declarations
  for (const m of src.matchAll(/\bfunction\s+([A-Za-z_$][\w$]*)/g)) declared.add(m[1]);
  // var/let/const declarations (в т.ч. несколько через запятую — берём первое имя)
  for (const m of src.matchAll(/\b(?:var|let|const)\s+([A-Za-z_$][\w$]*)/g)) declared.add(m[1]);
  // window.X = / window.X = function
  for (const m of src.matchAll(/window\.([A-Za-z_$][\w$]*)\s*=/g)) declared.add(m[1]);
  // объектные ключи вида `name:` или `name,` внутри литералов (эвристика)
  for (const m of src.matchAll(/^\s{4,}([A-Za-z_$][\w$]*)\s*[,:]/gm)) declared.add(m[1]);
  // классы
  for (const m of src.matchAll(/\bclass\s+([A-Za-z_$][\w$]*)/g)) declared.add(m[1]);
}

// Ключи i18n и прочие строки не должны попадать: ищем вызовы только вне строк
// и комментариев (грубая, но рабочая зачистка).
function stripStringsAndComments(src) {
  return src
    .replace(/\/\*[\s\S]*?\*\//g, ' ')
    .replace(/(^|[^:\\])\/\/[^\n]*/g, '$1 ')
    .replace(/'(?:\\.|[^'\\])*'/g, "''")
    .replace(/"(?:\\.|[^"\\])*"/g, '""')
    .replace(/`(?:\\.|[^`\\])*`/g, '``');
}

const missing = new Map();
for (const [f, raw] of texts) {
  const src = stripStringsAndComments(raw);
  for (const m of src.matchAll(/(?<![.\w$])([A-Za-z_$][\w$]*)\s*\(/g)) {
    const name = m[1];
    if (BUILTINS.has(name)) continue;
    if (declared.has(name)) continue;
    const line = src.slice(0, m.index).split('\n').length;
    if (!missing.has(f)) missing.set(f, []);
    missing.get(f).push(`${line}: ${name}(`);
  }
}

let total = 0;
for (const [f, list] of missing) {
  console.log(`\n${path.relative(process.cwd(), f)}`);
  for (const l of list.slice(0, 40)) { console.log('  ' + l); total++; }
  if (list.length > 40) console.log(`  ... +${list.length - 40}`);
}
console.log(`\nfiles=${files.length} declared=${declared.size} suspicious_calls=${total}`);
