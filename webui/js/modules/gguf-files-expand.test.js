// gguf-files-expand.test.js — живой дефект (2026-10-07).
//
// ЖАЛОБА ОПЕРАТОРА: «на странице поиска моделей huggingface не даёт полностью
// развернуть список моделей доступных для скачивания» — в карточке репозитория
// было видно только 5 файлов, а строка «+N ещё файлов» была ПРОСТЫМ ТЕКСТОМ без
// обработчика: добраться до остальных файлов репозитория было нельзя вообще.
//
// ЧТО ПРОВЕРЯЕТСЯ:
//   1. в разметке есть ВСЕ файлы репозитория (а не только первые 5) — хвост
//      отрисован сразу, поэтому раскрытие мгновенное и не зависит от сети;
//   2. хвост по умолчанию СКРЫТ (display:none) — иначе карточки распухли бы;
//   3. есть КНОПКА (.gguf-files-more-btn) с числом скрытых файлов и подписью
//      «Показать все» — именно её отсутствие и было дефектом;
//   4. строки хвоста неотличимы по разметке от видимых: у них те же классы
//      .gguf-file-row / .gguf-download-file-btn / .gguf-file-progress, на которые
//      завязаны делегированные обработчики (иначе скачивание из хвоста не
//      работало бы, даже если файлы показать);
//   5. обработчик разворачивания существует в коде (gguf-renderer-detail.js) —
//      проверяем по исходнику, потому что это делегирование на уровне документа.
//
// Run: node webui/js/modules/gguf-files-expand.test.js

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const SRC_RENDER = path.join(__dirname, 'gguf-renderer-detail-render.js');
const SRC_DETAIL = path.join(__dirname, 'gguf-renderer-detail.js');

// Файлы тестового репозитория: 9 штук. Первые пять попадут в видимую часть
// (pickTopGgufFiles сортирует по приоритету кванта), остальные — в хвост.
const FILES = [
    { path: 'Qwen3.8-Flash-Q4_K_M.gguf', size: 4 * 1024 * 1024 * 1024 },
    { path: 'Qwen3.8-Flash-Q5_K_M.gguf', size: 5 * 1024 * 1024 * 1024 },
    { path: 'Qwen3.8-Flash-Q6_K.gguf', size: 6 * 1024 * 1024 * 1024 },
    { path: 'Qwen3.8-Flash-Q8_0.gguf', size: 8 * 1024 * 1024 * 1024 },
    { path: 'Qwen3.8-Flash-IQ4_XS.gguf', size: 3 * 1024 * 1024 * 1024 },
    { path: 'Qwen3.8-Flash-IQ2_XS-00002-of-00002.gguf', size: 26 * 1024 * 1024 * 1024 },
    { path: 'Qwen3.8-Flash-IQ3_S-00002-of-00002.gguf', size: 26 * 1024 * 1024 * 1024 },
    { path: 'Qwen3.8-Flash-IQ3_XXS-00002-of-00002.gguf', size: 26 * 1024 * 1024 * 1024 },
    { path: 'Qwen3.8-Flash-Q2_0-00002-of-00002.gguf', size: 26 * 1024 * 1024 * 1024 },
];

const sandbox = {
    console: console,
    setTimeout: setTimeout,
    clearTimeout: clearTimeout,
    document: { addEventListener: function () {}, querySelectorAll: function () { return []; } },
    localStorage: { getItem: function () { return null; }, setItem: function () {} },
};
sandbox.window = sandbox;
sandbox.window.MonitorApp = {};

// Заглушки зависимостей рендера: экранирование, i18n, состояние.
sandbox.Utils = {
    escapeHtml: function (s) {
        return String(s == null ? '' : s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    },
};
sandbox._ = function (key) { return key; };
sandbox.M = sandbox.window.GgufModule = {
    // Рендер берёт i18n-хелпер из модуля (`const _ = M._`), а не из global.
    _: function (key) { return key; },
    state: {
        hfSearchQuery: 'qwen3.8',
        hfSearchResults: [
            {
                id: 'ISTA-DASLab/Qwen3.8-Flash-Next-GSQ-RCO-GGUF',
                downloads: 3000123,
                likes: 675,
                lastModified: '2026-10-01T00:00:00Z',
                hasGguf: true,
                totalSize: 274.6 * 1024 * 1024 * 1024,
                recommended: 'Qwen3.8-Flash-Q4_K_M.gguf',
                files: FILES,
            },
        ],
        hfSearchSelected: null,
        hfSearchLoading: false,
    },
};

vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(SRC_RENDER, 'utf8'), sandbox, { filename: SRC_RENDER });

const M = sandbox.window.GgufModule;
assert.strictEqual(typeof M.renderHuggingFacePane, 'function',
    'gguf-renderer-detail-render.js обязан экспортировать renderHuggingFacePane');

const html = M.renderHuggingFacePane();
assert.ok(html && html.length > 0, 'рендер панели HuggingFace вернул пустоту');

// --- 1. Все файлы попали в разметку -----------------------------------------
const allRows = (html.match(/gguf-file-row/g) || []).length;
assert.strictEqual(allRows, FILES.length,
    'в разметке должно быть ВСЕ ' + FILES.length + ' файлов репозитория, а не только 5 видимых');

// --- 2. Хвост скрыт по умолчанию --------------------------------------------
const extraIdx = html.indexOf('gguf-files-extra');
assert.notStrictEqual(extraIdx, -1,
    'нет контейнера .gguf-files-extra — скрытого хвоста, который раскрывает кнопка');
const extraChunk = html.slice(extraIdx, html.indexOf('gguf-files-more-btn'));
assert.ok(/gguf-files-extra"[^>]*style="[^"]*display:\s*none/.test(html.slice(Math.max(0, extraIdx - 60), extraIdx + 60)),
    'хвост .gguf-files-extra обязан быть скрыт по умолчанию (display:none)');
assert.ok(extraChunk.indexOf('gguf-download-file-btn') !== -1,
    'в скрытом хвосте нет кнопок скачивания — файлы нельзя будет скачать после раскрытия');

// --- 3. Кнопка раскрытия с числом скрытых файлов ----------------------------
assert.ok(html.indexOf('gguf-files-more-btn') !== -1,
    'НЕТ кнопки .gguf-files-more-btn — это и есть дефект: «+N ещё файлов» было текстом без обработчика');
const leftover = FILES.length - 5; // 5 видимых по умолчанию
assert.ok(html.indexOf('+' + leftover + ' ') !== -1,
    'в подписи кнопки нет количества скрытых файлов (ожидалось +' + leftover + ')');

// --- 4. Разметка строк хвоста совпадает с видимыми --------------------------
const downloadBtns = (html.match(/gguf-download-file-btn/g) || []).length;
assert.strictEqual(downloadBtns, FILES.length,
    'кнопок скачивания должно быть столько же, сколько файлов (обработчик ищет .gguf-download-file-btn)');
const progressBlocks = (html.match(/gguf-file-progress/g) || []).length;
assert.ok(progressBlocks >= FILES.length,
    'у каждой строки обязан быть блок прогресса .gguf-file-progress');

// --- 5. Обработчик раскрытия есть в коде ------------------------------------
const detailSrc = fs.readFileSync(SRC_DETAIL, 'utf8');
assert.ok(detailSrc.indexOf("closest('.gguf-files-more-btn')") !== -1,
    'в gguf-renderer-detail.js нет делегированного обработчика .gguf-files-more-btn — кнопка была бы мёртвой');
assert.ok(detailSrc.indexOf("querySelector('.gguf-files-extra')") !== -1,
    'обработчик не находит .gguf-files-extra, раскрывать нечего');
// «Свернуть все» обязано сворачивать и раскрытые хвосты, иначе карточка после
// повторного раскрытия выглядела бы уже развёрнутой.
assert.ok(/collapseAll[\s\S]{0,700}gguf-files-extra/.test(detailSrc),
    '«Свернуть все» не сворачивает раскрытые хвосты');

console.log('ok 1 — в разметке все ' + FILES.length + ' файлов (не только 5)');
console.log('ok 2 — хвост скрыт по умолчанию (display:none)');
console.log('ok 3 — есть кнопка раскрытия с числом скрытых файлов (+' + leftover + ')');
console.log('ok 4 — разметка строк хвоста совпадает с видимыми (скачивание работает)');
console.log('ok 5 — обработчик раскрытия и сворачивания существует в коде');
console.log('\ngguf-files-expand: все проверки пройдены');
