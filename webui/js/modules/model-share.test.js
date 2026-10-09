// model-share.test.js — R89 (2026-10-08): «Поделиться моделью» (перенос между
// бэкендами). Проверяем чистый слой: выбор целей, тело запроса, отображение
// прогресса и признак завершения задания.
//
// Запуск: node webui/js/modules/model-share.test.js
//
// ПОЧЕМУ ЭТО ВАЖНО. Оператор запускает перенос 14 ГБ и смотрит на прогресс:
//   - в список целей не должны попадать бэкенды ДРУГОГО типа (у image-воркера
//     каталог bundle, у текстового — файл .gguf; перенос между ними невозможен)
//     и сам источник;
//   - 100% нельзя показывать, пока перенос идёт (tar-заголовки дают перебег
//     байтов, и «100% на идущем переносе» читается как «зависло»).
'use strict';

const assert = require('assert');

global.window = global;
global.document = { getElementById() { return null; }, createElement() { return { style: {}, addEventListener() {}, appendChild() {} }; }, body: { appendChild() {} }, querySelectorAll() { return []; } };
global.localStorage = { getItem() { return null; }, setItem() {}, removeItem() {} };
global.WEBUI_CONFIG = { API_BASE: '', API_TOKEN: 'test-key' };

require('./model-share.js');
const MS = window.ModelShare;
assert.ok(MS && MS.pure, 'window.ModelShare.pure должен быть экспортирован');
const P = MS.pure;

let passed = 0;
function check(name, fn) {
    fn();
    passed++;
    console.log('  \u2713 ' + name);
}

const backends = [
    { id: 'cppworker-gpu', type: 'llama_cpp', host: 'cppworker-gpu' },
    { id: 'CPPWORKER-34', type: 'llama_cpp', host: '192.0.2.11' },
    { id: 'imageworker', type: 'image_cpp', host: 'imageworker' },
    { id: 'IMAGEWORKER-34', type: 'image_cpp', host: '192.0.2.11' },
    { id: 'legacy', backendType: 'ollama', host: 'legacy' },
];

check('kindOf: image_cpp → image, llama_cpp/ollama → text, неизвестное → пусто', function () {
    assert.strictEqual(P.kindOf({ type: 'image_cpp' }), 'image');
    assert.strictEqual(P.kindOf({ backendType: 'llama_cpp' }), 'text');
    assert.strictEqual(P.kindOf({ type: 'ollama' }), 'text');
    assert.strictEqual(P.kindOf({ type: 'unknown' }), '');
    assert.strictEqual(P.kindOf(null), '');
});

check('candidateBackends: только тот же тип и не источник', function () {
    const text = P.candidateBackends(backends, 'cppworker-gpu', 'text').map(b => b.id);
    assert.deepStrictEqual(text.sort(), ['CPPWORKER-34', 'legacy']);

    const image = P.candidateBackends(backends, 'imageworker', 'image').map(b => b.id);
    assert.deepStrictEqual(image, ['IMAGEWORKER-34']);

    // Тип берётся и из самого источника, если kind не передан.
    const auto = P.candidateBackends(backends, 'imageworker').map(b => b.id);
    assert.deepStrictEqual(auto, ['IMAGEWORKER-34']);
});

check('candidateBackends: нет кандидатов — пустой список, а не сам источник', function () {
    const only = [{ id: 'solo', type: 'image_cpp' }];
    assert.deepStrictEqual(P.candidateBackends(only, 'solo', 'image'), []);
});

check('buildShareRequest: тело запроса к балансеру', function () {
    const body = P.buildShareRequest('imageworker', 'qwen-image-2.1-uncensored-gguf', ['IMAGEWORKER-34'], false);
    assert.deepStrictEqual(body, {
        source: 'imageworker',
        model: 'qwen-image-2.1-uncensored-gguf',
        targets: ['IMAGEWORKER-34'],
        overwrite: false,
    });
    assert.strictEqual(P.buildShareRequest(null, null, null, 1).overwrite, true);
    assert.deepStrictEqual(P.buildShareRequest('a', 'm', null, false).targets, []);
});

check('targetView: идущий перенос НЕ показывает 100% (tar даёт перебег байтов)', function () {
    const v = P.targetView({ state: 'running', percent: 100, bytes: 1000, total: 990, speedBps: 5 * 1024 * 1024 });
    assert.ok(v.percent <= 99, 'percent=' + v.percent + ' — иначе прогресс читается как «зависло»');
    assert.ok(v.detail.indexOf('MiB/s') !== -1, 'нет скорости: ' + v.detail);
    assert.strictEqual(v.label, 'копируется');
});

check('targetView: завершённая цель — ровно 100% и объём', function () {
    const v = P.targetView({ state: 'done', percent: 42, bytes: 14 * 1024 * 1024 * 1024, total: 14 * 1024 * 1024 * 1024 });
    assert.strictEqual(v.percent, 100);
    assert.strictEqual(v.label, 'готово');
    assert.ok(/GiB/.test(v.detail), 'ожидался объём в GiB: ' + v.detail);
});

check('targetView: пропуск и ошибка объясняются текстом с сервера', function () {
    const skipped = P.targetView({ state: 'skipped', note: 'модель уже есть на этом бэкенде' });
    assert.strictEqual(skipped.percent, 100);
    assert.strictEqual(skipped.detail, 'модель уже есть на этом бэкенде');
    const failed = P.targetView({ state: 'failed', error: 'HTTP 409: already exists' });
    assert.strictEqual(failed.label, 'ошибка');
    assert.strictEqual(failed.detail, 'HTTP 409: already exists');
});

check('targetView: пустая/битая цель не роняет отрисовку', function () {
    const v = P.targetView(null);
    assert.strictEqual(v.percent, 0);
    assert.strictEqual(v.state, 'pending');
    assert.strictEqual(P.targetView({ state: 'running', percent: NaN }).percent, 0);
});

check('jobFinished: активным считается только running', function () {
    assert.strictEqual(P.jobFinished({ state: 'running' }), false);
    ['done', 'partial', 'failed', 'canceled'].forEach(function (s) {
        assert.strictEqual(P.jobFinished({ state: s }), true, s + ' должен считаться завершённым');
    });
    assert.strictEqual(P.jobFinished(null), false);
});

check('formatBytes/formatSpeed: человеческие единицы', function () {
    assert.strictEqual(P.formatBytes(0), '0 B');
    assert.strictEqual(P.formatBytes(1024), '1.0 KiB');
    assert.strictEqual(P.formatBytes(1536), '1.5 KiB');
    assert.ok(/GiB/.test(P.formatBytes(5 * 1024 * 1024 * 1024)));
    assert.strictEqual(P.formatSpeed(0), '');
    assert.ok(/MiB\/s/.test(P.formatSpeed(10 * 1024 * 1024)));
});

console.log('\nmodel-share: ' + passed + ' проверок пройдено');
process.exit(0);
