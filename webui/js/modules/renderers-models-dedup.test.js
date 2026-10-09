// renderers-models-dedup.test.js — R91 (2026-10-09).
//
// Запуск: node webui/js/modules/renderers-models-dedup.test.js
//
// ЖАЛОБА ОПЕРАТОРА (скриншот страницы «Загруженные модели»): модель
// Qwen3-Instruct-2507-q4km показана ДВУМЯ карточками одного и того же бэкенда
// cppworker-gpu-bundled-agent — одна с прочерками и «⌛ Истёк» («сведения ещё не
// получены»), вторая с реальными данными (2.3 GB, GGUF Path, RAM 2620/25044).
//
// Причина (найдена на живом стенде): балансер отдавал одну и ту же модель в ДВУХ
// полях одного бэкенда —
//
//   ollama.runningModels: [{name:"Qwen3-Instruct-2507-q4km", size:0, vramUsage:0,
//                           expiresAt:"0001-01-01T00:00:00Z", ...}]
//   llamaCpp.loadedModels: [{name:"Qwen3-Instruct-2507-q4km",
//                            path:"/app/models/Qwen3-Instruct-2507-q4km.gguf",
//                            size:2497281120, vramUsage:4961, contextLength:32768,
//                            quantization:"Q4_K_M", architecture:"qwen3", ...}]
//
// Первое — Ollama-совместимая проекция (нужна для /api/ps, у неё только имя),
// второе — реальные данные поллера. Страница складывала оба списка в одну сетку.
//
// Тест фиксирует защиту на стороне WebUI (нужна и потому, что образы WebUI и
// балансера обновляются отдельно):
//   1. дубли по (бэкенд, имя) сводятся в одну карточку, остаётся запись с данными;
//   2. регистр и .gguf не делают из одной модели две;
//   3. модели разных бэкендов с одинаковым именем НЕ схлопываются;
//   4. нулевая метка времени Go (0001-01-01) — это «срока нет», а не «истёк»;
//   5. реальный срок в прошлом по-прежнему показывается как «истёк».

'use strict';

const assert = require('assert');

// --- минимальный DOM (тот же приём, что в renderers-queue.test.js) ------------
function makeEl(id) {
    return {
        id: id || '',
        innerHTML: '',
        textContent: '',
        value: '',
        hidden: false,
        style: {},
        dataset: {},
        classList: { add() {}, remove() {}, contains() { return false; } },
        setAttribute() {},
        getAttribute() { return null; },
        appendChild() {},
        removeChild() {},
        addEventListener() {},
        querySelector() { return null; },
        querySelectorAll() { return []; },
    };
}
const byId = {};
global.document = {
    createElement() {
        let text = '';
        const el = makeEl('');
        Object.defineProperty(el, 'textContent', { get() { return text; }, set(v) { text = String(v); } });
        Object.defineProperty(el, 'innerHTML', {
            get() {
                return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
            },
        });
        return el;
    },
    getElementById(id) { return byId[id] || (byId[id] = makeEl(id)); },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    addEventListener() {},
    removeEventListener() {},
};
global.window = global;

const I18N_MAP = {
    'renderers.expired': 'Истёк',
    'models.no_models': 'Нет моделей',
};
global.window.I18N = {
    t(k, vars) {
        let s = Object.prototype.hasOwnProperty.call(I18N_MAP, k) ? I18N_MAP[k] : k;
        if (vars) Object.keys(vars).forEach(function (n) { s = s.split('{' + n + '}').join(String(vars[n])); });
        return s;
    },
    getLang() { return 'ru'; },
};

require('./utils.js');
global.Utils = global.window.Utils;
require('./renderers.js');
const R = global.window.Renderers;
assert.ok(R && typeof R.dedupModelCards === 'function', 'Renderers.dedupModelCards должен быть экспортирован');

let checks = 0;
function check(name, fn) {
    fn();
    checks++;
    console.log('  ✓ ' + name);
}

// Живые данные стенда (сокращённо).
const BACKEND = 'cppworker-gpu-bundled-agent';
const OLLAMA_PROJECTION = {
    name: 'Qwen3-Instruct-2507-q4km',
    size: 0,
    vramUsage: 0,
    ramUsage: 0,
    expiresAt: '0001-01-01T00:00:00Z',
    digest: '',
    quantization: '',
    backend: BACKEND,
    backendStatus: 'healthy',
    backendType: 'llama_cpp',
};
const LOADED_MODELS_ENTRY = {
    name: 'Qwen3-Instruct-2507-q4km',
    size: 2497281120,
    vramUsage: 4961,
    ramUsage: 2620,
    expiresAt: '',
    ggufPath: '/app/models/Qwen3-Instruct-2507-q4km.gguf',
    contextLength: 32768,
    quantization: 'Q4_K_M',
    architecture: 'qwen3',
    backend: BACKEND,
    backendStatus: 'healthy',
    backendType: 'llama_cpp',
};

check('одна модель одного бэкенда — одна карточка (проекция + loadedModels)', function () {
    const out = R.dedupModelCards([OLLAMA_PROJECTION, LOADED_MODELS_ENTRY]);
    assert.strictEqual(out.length, 1, 'две записи об одной модели обязаны свестись в одну');
    assert.strictEqual(out[0].size, 2497281120, 'осталась запись без данных — карточка снова будет с прочерками');
    assert.strictEqual(out[0].ggufPath, '/app/models/Qwen3-Instruct-2507-q4km.gguf');
    assert.strictEqual(out[0].contextLength, 32768);
});

check('порядок источников не важен (loadedModels первым)', function () {
    const out = R.dedupModelCards([LOADED_MODELS_ENTRY, OLLAMA_PROJECTION]);
    assert.strictEqual(out.length, 1);
    assert.strictEqual(out[0].size, 2497281120);
});

check('регистр и .gguf не делают из одной модели две', function () {
    const a = Object.assign({}, LOADED_MODELS_ENTRY);
    const b = Object.assign({}, OLLAMA_PROJECTION, { name: 'qwen3-instruct-2507-Q4KM' });
    const c = Object.assign({}, OLLAMA_PROJECTION, { name: 'Qwen3-Instruct-2507-q4km.gguf' });
    assert.strictEqual(R.dedupModelCards([a, b, c]).length, 1,
        'одна модель в трёх написаниях обязана остаться одной карточкой');
});

check('модели разных бэкендов с одинаковым именем НЕ схлопываются', function () {
    const other = Object.assign({}, LOADED_MODELS_ENTRY, { backend: 'CPPWORKER-34' });
    const out = R.dedupModelCards([OLLAMA_PROJECTION, LOADED_MODELS_ENTRY, other]);
    assert.strictEqual(out.length, 2, 'у каждой машины своя карточка одной и той же модели');
    const backends = out.map(function (m) { return m.backend; }).sort();
    assert.deepStrictEqual(backends, ['CPPWORKER-34', 'cppworker-gpu-bundled-agent']);
});

check('разные модели одного бэкенда сохраняются полностью', function () {
    const gemma = Object.assign({}, LOADED_MODELS_ENTRY, { name: 'gemma-4-E4B-it-Q4_K_M', size: 4215695776 });
    const out = R.dedupModelCards([LOADED_MODELS_ENTRY, gemma, OLLAMA_PROJECTION]);
    assert.strictEqual(out.length, 2, 'сведение дублей не должно удалять другие модели');
});

check('нулевая метка времени Go — «срока нет», а не «истёк»', function () {
    assert.strictEqual(R.isZeroTimestamp('0001-01-01T00:00:00Z'), true);
    assert.strictEqual(R.isZeroTimestamp('0001-01-01'), true);
    assert.strictEqual(R.isZeroTimestamp(''), true);
    assert.strictEqual(R.isZeroTimestamp(null), true);
    assert.strictEqual(R.isZeroTimestamp('не дата'), true);
    assert.strictEqual(R.isZeroTimestamp(new Date(Date.now() + 3600e3).toISOString()), false);
});

check('проекция без данных не показывает «⌛ Истёк» (нулевая метка времени)', function () {
    // Это ровно карточка со скриншота: бэкенд llama.cpp, пришла только
    // Ollama-проекция (loadedModels ещё пуст), expiresAt = 0001-01-01.
    const backends = [{
        id: BACKEND,
        status: 'healthy',
        backendType: 'llama_cpp',
        host: '127.0.0.1',
        gpu: { memoryTotal: 8192, memoryUsed: 5000, memoryFree: 3192 },
        system: { memoryTotal: 25044, memoryUsed: 3000, memoryFree: 22044 },
        ollama: { runningModels: [OLLAMA_PROJECTION] },
        llamaCpp: { loadedModels: [] },
    }];
    R.modelsPage(backends);
    const grid = byId['modelsGrid'].innerHTML;
    assert.ok(grid.indexOf('Qwen3-Instruct-2507-q4km') !== -1, 'карточка модели не отрисована');
    assert.ok(grid.indexOf('Истёк') === -1,
        'нулевая метка времени (0001-01-01) показана как «⌛ Истёк» — модель загружена, а карточка говорит обратное');
});

check('настоящий срок в прошлом по-прежнему показывается как «Истёк» (Ollama)', function () {
    // У Ollama-бэкенда expiresAt настоящий: просроченная модель обязана быть видна.
    const expired = {
        name: 'llama3.1:8b',
        size: 4 * 1024 * 1024 * 1024,
        expiresAt: new Date(Date.now() - 3600e3).toISOString(),
    };
    const backends = [{
        id: 'ollama-1',
        status: 'healthy',
        backendType: 'ollama',
        ollama: { runningModels: [expired] },
    }];
    R.modelsPage(backends);
    assert.ok(byId['modelsGrid'].innerHTML.indexOf('Истёк') !== -1,
        'настоящий срок в прошлом обязан показываться как «Истёк»');
});

check('счётчики страницы считают модели после сведения дублей', function () {
    const backends = [{
        id: BACKEND,
        status: 'healthy',
        backendType: 'llama_cpp',
        ollama: { runningModels: [OLLAMA_PROJECTION] },
        llamaCpp: { loadedModels: [LOADED_MODELS_ENTRY] },
    }];
    R.modelsPage(backends);
    assert.strictEqual(String(byId['modelsTotal'].textContent), '1',
        'одна модель посчитана дважды');
    assert.ok(byId['modelsGrid'].innerHTML.indexOf('2.3 GB') !== -1,
        'осталась карточка без данных: размер из loadedModels не попал в разметку');
});

console.log('\nrenderers-models-dedup: все проверки пройдены (' + checks + ')');
