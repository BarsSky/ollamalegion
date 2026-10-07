// gguf-about-vram.test.js — R-MultiHost (2026-10-07).
//
// Запуск: node webui/js/modules/gguf-about-vram.test.js
//
// ЧТО ПРОВЕРЯЕМ И ПОЧЕМУ.
//
// В панели «Инфо» страницы GGUF карточка GPU показывала «8.0 KB» на карте с
// 8 GB и «6.9 KB» свободных: значения приходили из BackendMetrics.GPU, где
// память измеряется в МЕГАБАЙТАХ (pkg/types/metrics.go: GPUMetrics.MemoryTotal /
// MemoryUsed / MemoryFree — «Всего VRAM (MB)»), а форматировались как БАЙТЫ.
// 8192 MB → 8192 байт → «8.0 KB».
//
// Теперь для VRAM есть отдельный formatVramMB; этот тест фиксирует и его, и то,
// что formatFileSize (для РАЗМЕРОВ ФАЙЛОВ, они действительно в байтах) не поехал.

'use strict';

const assert = require('assert');

global.window = global;
global.GgufModule = {};

require('./gguf-renderer-helpers.js');
const M = global.GgufModule;

assert.strictEqual(typeof M.formatVramMB, 'function', 'formatVramMB должен быть экспортирован');
assert.strictEqual(typeof M.formatFileSize, 'function', 'formatFileSize должен остаться');

let checks = 0;
function check(name, fn) {
    fn();
    checks++;
    console.log('  ✓ ' + name);
}

// Реальные значения с живой стойки: RTX 3070 8 GB, свободно 7097 MB.
check('VRAM 8192 MB -> 8.0 GB (а не «8.0 KB»)', function () {
    assert.strictEqual(M.formatVramMB(8192), '8.0 GB');
});

check('VRAM 7097 MB -> 6.9 GB', function () {
    assert.strictEqual(M.formatVramMB(7097), '6.9 GB');
});

check('VRAM 24576 MB -> 24.0 GB (карта на 24 GB)', function () {
    assert.strictEqual(M.formatVramMB(24576), '24.0 GB');
});

check('маленькие значения остаются читаемыми', function () {
    assert.strictEqual(M.formatVramMB(512), '512.0 MB');
    assert.strictEqual(M.formatVramMB(1), '1.0 MB');
});

check('0 и мусор -> прочерк, а не «0 B»', function () {
    // У бэкенда без NVIDIA-метрик честнее показать «неизвестно», чем ноль.
    assert.strictEqual(M.formatVramMB(0), '-');
    assert.strictEqual(M.formatVramMB(null), '-');
    assert.strictEqual(M.formatVramMB(undefined), '-');
    assert.strictEqual(M.formatVramMB('abc'), '-');
    assert.strictEqual(M.formatVramMB(-5), '-');
});

check('строковое число тоже принимается (JSON иногда приходит строкой)', function () {
    assert.strictEqual(M.formatVramMB('8192'), '8.0 GB');
});

check('formatFileSize по-прежнему работает с БАЙТАМИ (размеры файлов)', function () {
    assert.strictEqual(M.formatFileSize(1536 * 1024 * 1024), '1.5 GB');
    assert.strictEqual(M.formatFileSize(320 * 1024 * 1024), '320.0 MB');
    assert.strictEqual(M.formatFileSize(0), '0 B');
});

// Проверка «двух одинаковых чисел — разных единиц»: именно эта пара и была
// причиной жалобы «нет достоверной информации».
check('8192 как МБ VRAM и 8192 как байты — разные подписи', function () {
    assert.strictEqual(M.formatVramMB(8192), '8.0 GB');
    assert.strictEqual(M.formatFileSize(8192), '8.0 KB');
    assert.notStrictEqual(M.formatVramMB(8192), M.formatFileSize(8192));
});

console.log('\nOK: ' + checks + ' checks passed');
