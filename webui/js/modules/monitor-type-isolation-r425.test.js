// monitor-type-isolation-r425.test.js — R425 (2026-11-03): тип бэкенда в мониторе.
//
// ЖИВОЙ ДЕФЕКТ (монитор стенда RTX 3070). В очереди стоял запрос к
// llama.cpp-модели, а баннер предлагал «перераспределить» нагрузку, называя
// свободным imageworker (image.cpp, 0/64 слотов). Нажатие такого совета ведёт к
// перекладке текстовой модели на image-бэкенд — заведомо ошибка, типы
// несовместимы (R-Image: backend type isolation).
//
// Контракт, который фиксирует тест:
//  1. тип бэкенда читается из payload /api/v1/cluster (`backendType`), с
//     фолбэком на `type` и `engine` — поле `type` в cluster-снимке пустое;
//  2. «свободные» бэкенды для ТЕКСТОВОЙ очереди не включают image-бэкенды;
//  3. альтернативой для сессии может быть только бэкенд того же типа;
//  4. fitText обрезает текст по ширине (а не по числу символов) — длинное имя
//     модели не вылезает за карточку клиента.
//
// Запуск: node webui/js/modules/monitor-type-isolation-r425.test.js

'use strict';

const assert = require('assert');

global.window = global;
global.window.WEBUI_CONFIG = {};
global.localStorage = { getItem() { return null; }, setItem() {}, removeItem() {} };
global.document = {
    getElementById() { return null; },
    addEventListener() {},
    removeEventListener() {},
    documentElement: { getAttribute() { return null; } },
    querySelectorAll() { return []; },
    createElement() { return { style: {}, classList: { add() {}, remove() {} }, appendChild() {} }; },
};
global.window.location = { search: '', origin: 'http://127.0.0.1:18083', hash: '' };
global.location = global.window.location;
global.window.addEventListener = function () {};
global.window.removeEventListener = function () {};
global.addEventListener = global.window.addEventListener;
global.removeEventListener = global.window.removeEventListener;

require('../monitor/state.js');
const MA = global.window.MonitorApp;
assert.ok(MA, 'MonitorApp должен существовать');

let checks = 0;
function check(name, fn) {
    fn();
    checks++;
    console.log('  ✓ ' + name);
}

// --- фикстуры: ровно тот случай, что на стенде -----------------------------
const llamaBusy = {
    id: 'cppworker-gpu-bundled-agent',
    backendType: 'llama_cpp',
    engine: 'llama_cpp',
    status: 'healthy',
    activeRequests: 1,
    maxConcurrentRequests: 1,
    models: [{ name: 'Qwen3-4B-Instruct-2507-Q4_K_M' }]
};
const imageIdle = {
    id: 'imageworker',
    backendType: 'image_cpp',
    engine: 'image_cpp',
    status: 'healthy',
    activeRequests: 0,
    maxConcurrentRequests: 64
};
const llamaIdle = {
    id: 'cppworker-2',
    backendType: 'llama_cpp',
    status: 'healthy',
    activeRequests: 0,
    maxConcurrentRequests: 1
};

check('тип бэкенда: backendType → type → engine', function () {
    assert.strictEqual(MA.backendTypeOf(llamaBusy), 'llama_cpp');
    assert.strictEqual(MA.backendTypeOf(imageIdle), 'image_cpp');
    assert.strictEqual(MA.backendTypeOf({ type: 'image_cpp' }), 'image_cpp');
    assert.strictEqual(MA.backendTypeOf({ engine: 'image_cpp' }), 'image_cpp');
    assert.strictEqual(MA.backendTypeOf(null), '');
});

check('image-бэкенд распознаётся отдельно', function () {
    assert.strictEqual(MA.isImageBackend(imageIdle), true);
    assert.strictEqual(MA.isImageBackend(llamaBusy), false);
});

check('свободные для текстовой очереди НЕ включают imageworker', function () {
    const free = MA.freeTextBackends([llamaBusy, imageIdle, llamaIdle], 0.5);
    const ids = free.map(b => b.id);
    assert.deepStrictEqual(ids, ['cppworker-2'],
        'свободным должен считаться только свободный ТЕКСТОВЫЙ бэкенд, получено: ' + ids.join(', '));
});

check('альтернативой для сессии может быть только тот же тип', function () {
    const alt = MA.altBackendsFor([llamaBusy, imageIdle, llamaIdle], llamaBusy, 0.5);
    const ids = alt.map(b => b.id);
    assert.deepStrictEqual(ids, ['cppworker-2'],
        'imageworker не может быть альтернативой llama.cpp-сессии, получено: ' + ids.join(', '));
    // и наоборот: для image-сессии текстовый бэкенд не альтернатива
    const imgAlt = MA.altBackendsFor([llamaBusy, imageIdle, llamaIdle], imageIdle, 0.5);
    assert.deepStrictEqual(imgAlt.map(b => b.id), [], 'llama.cpp не может быть альтернативой image-сессии');
});

check('мёртвый бэкенд не считается свободным', function () {
    const dead = { id: 'cppworker-3', backendType: 'llama_cpp', status: 'offline', activeRequests: 0, maxConcurrentRequests: 1 };
    assert.deepStrictEqual(MA.freeTextBackends([dead], 0.5).map(b => b.id), []);
});

check('fitText обрезает по ширине и ставит многоточие', function () {
    // Фейковый 2D-контекст: 7 px на символ — как у 10-11px моноширинного шрифта.
    const ctx = { measureText: (s) => ({ width: String(s).length * 7 }) };
    const model = 'Qwen3-4B-Instruct-2507-Q4_K_M';
    const out = MA.fitText(ctx, model, 140);
    assert.ok(ctx.measureText(out).width <= 140, 'строка шире плитки: ' + out);
    assert.ok(out.endsWith('…'), 'обрезанная строка должна кончаться многоточием: ' + out);
    assert.ok(out.length < model.length, 'строка не обрезана: ' + out);
});

check('fitText не трогает текст, который влезает', function () {
    const ctx = { measureText: (s) => ({ width: String(s).length * 7 }) };
    assert.strictEqual(MA.fitText(ctx, 'OK', 140), 'OK');
    assert.strictEqual(MA.fitText(ctx, '', 140), '');
});

console.log('monitor-type-isolation-r425: ' + checks + ' проверок OK');
