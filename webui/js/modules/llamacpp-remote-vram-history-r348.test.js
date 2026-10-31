// llamacpp-remote-vram-history-r348.test.js — R348 (2026-10-31).
//
// Запуск: node webui/js/modules/llamacpp-remote-vram-history-r348.test.js
//
// ОСНОВАНИЕ. docs/plans/2026-10-31-two-node-donor-check.md: на двухузловом стенде
// донор доступен и раскладка работает (23 слоя локально + 19 по RPC), но снимок
// узлов живёт 30 с, после чего cppworker честно отдаёт vram_remote_* = 0 (R132:
// протухшее не выдаём за текущую ёмкость). В панели это выглядело как «узлов в
// раскладке нет» — вопрос «влезет ли модель в две карты» оставался без ответа.
//
// R348 добавляет ОТДЕЛЬНЫЕ поля истории (vramRemoteLastKnown*), и панель обязана:
//   1. показывать историю, когда текущего вклада нет, — ВСЕГДА с возрастом;
//   2. не показывать историю, когда есть текущий замер (два числа рядом читались бы
//      как противоречие);
//   3. не показывать историю при выключенной раскладке (узлы не используются, R115);
//   4. не показывать ничего, если история нулевая (нули — это «не измеряли»).
//
// Почему возраст обязателен: без «12 мин назад» старые данные принимаются за свежие,
// а именно это и есть исходный дефект класса «панель врёт числом».

'use strict';

const assert = require('assert');

// --- минимальный DOM ---------------------------------------------------------
function makeEl(id) {
    return {
        id: id || '', innerHTML: '', textContent: '', value: '', hidden: false,
        style: {}, dataset: {}, children: [],
        classList: { add() {}, remove() {}, contains() { return false; } },
        setAttribute() {}, getAttribute() { return null; },
        appendChild() {}, removeChild() {}, remove() {},
        addEventListener() {}, querySelector() { return null; }, querySelectorAll() { return []; },
    };
}
const byId = {};
global.document = {
    createElement() {
        let text = '';
        const el = makeEl('');
        Object.defineProperty(el, 'textContent', { get() { return text; }, set(v) { text = String(v); } });
        Object.defineProperty(el, 'innerHTML', {
            get() { return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'); },
        });
        return el;
    },
    getElementById(id) { return byId[id] || (byId[id] = makeEl(id)); },
    querySelector() { return null; }, querySelectorAll() { return []; },
    addEventListener() {}, removeEventListener() {}, body: { appendChild() {} },
};
global.window = global;

// --- i18n (реальные подписи из ru.js) ----------------------------------------
const I18N_MAP = {
    'renderers.section_vram': '🎛️ VRAM',
    'renderers.vram_total': 'Всего VRAM',
    'renderers.free_vram': 'Свободно VRAM',
    'renderers.vram_remote_free': 'Свободно VRAM (RPC-узлы)',
    'renderers.vram_remote_nodes': 'узлов',
    'renderers.vram_remote_last_known': 'Свободно VRAM (RPC-узлы, последний замер)',
    'renderers.vram_remote_last_hint': 'Снимок узлов обновляется при загрузке/раскладке; текущий вклад сейчас считается устаревшим',
    'renderers.ago': 'назад',
    'renderers.age_just_now': 'только что',
    'renderers.age_sec': 'с',
    'renderers.age_min': 'мин',
    'renderers.age_hour': 'ч',
    'renderers.section_llama_cpp_params': 'Параметры llama.cpp',
    'renderers.context': 'Контекст',
    'renderers.section_loaded_models': '🤖 Загруженные модели',
};
function tr(key, vars) {
    let s = Object.prototype.hasOwnProperty.call(I18N_MAP, key) ? I18N_MAP[key] : key;
    if (vars && typeof vars === 'object') {
        Object.keys(vars).forEach(function (k) { s = s.split('{' + k + '}').join(String(vars[k])); });
    }
    return s;
}
global.window.I18N = { t: tr, getLang() { return 'ru'; } };

require('./utils.js');
global.Utils = global.window.Utils;
require('./renderers.js');
const R = global.window.Renderers;
assert.ok(R && typeof R.backendsPage === 'function', 'renderers.js должен экспортировать Renderers.backendsPage');

let passed = 0;
function check(name, fn) { fn(); passed++; console.log('  \u2713 ' + name); }

function renderBackendsPage(backends) {
    byId['backendsManageBody'] = makeEl('backendsManageBody');
    R.backendsPage(backends);
    return byId['backendsManageBody'].innerHTML;
}

function cppBackend(llamaCpp) {
    return {
        id: 'cppworker-gpu-bundled-agent',
        backendType: 'llama_cpp',
        type: 'llama_cpp',
        status: 'healthy',
        host: 'cppworker-gpu',
        cppWorkerPort: 18092,
        llamaCpp: llamaCpp,
    };
}

console.log('R348: история удалённой VRAM в панели (llama.cpp)');

// --- 1. Текущего вклада нет, история есть -----------------------------------
check('Нет текущего замера: показывается последний, с возрастом', function () {
    const html = renderBackendsPage([cppBackend({
        totalVramMb: 8191, availableVramMb: 7073,
        rpcEnabled: true,
        vramRemoteFreeMb: 0, vramRemoteNodes: 0, vramRemoteSource: 'none',
        vramRemoteLastKnownFreeMb: 7019, vramRemoteLastKnownNodes: 1,
        vramRemoteLastKnownAgeSec: 732,
        loadedModels: [],
    })]);
    assert.ok(html.indexOf('Свободно VRAM (RPC-узлы, последний замер)') !== -1,
        'нет строки истории: ' + html.slice(0, 1600));
    assert.ok(html.indexOf('6.9 GB') !== -1, 'нет значения свободной VRAM донора (7019 МБ → 6.9 GB)');
    assert.ok(html.indexOf('1 узлов') !== -1, 'нет числа узлов');
    assert.ok(html.indexOf('назад 12 мин') !== -1,
        'нет возраста замера (732 с → 12 мин): без него старые данные примут за свежие');
    assert.ok(html.indexOf('Свободно VRAM (RPC-узлы)<') === -1,
        'строка истории не должна называться текущим замером');
});

// --- 2. Текущий замер есть — историю не дублируем ---------------------------
check('Есть текущий замер: показывается он, истории нет', function () {
    const html = renderBackendsPage([cppBackend({
        totalVramMb: 8191, availableVramMb: 7073,
        rpcEnabled: true,
        vramRemoteFreeMb: 7000, vramRemoteNodes: 1,
        vramRemoteLastKnownFreeMb: 7019, vramRemoteLastKnownNodes: 1,
        vramRemoteLastKnownAgeSec: 30,
        loadedModels: [],
    })]);
    assert.ok(html.indexOf('Свободно VRAM (RPC-узлы)<') !== -1, 'нет текущей строки RPC-узлов');
    assert.strictEqual(html.indexOf('последний замер'), -1,
        'история показана рядом с текущим замером — два числа читались бы как противоречие');
});

// --- 3. Раскладка выключена — истории нет -----------------------------------
check('rpcEnabled=false: истории нет (узлы не используются)', function () {
    const html = renderBackendsPage([cppBackend({
        totalVramMb: 8191, availableVramMb: 7073,
        rpcEnabled: false,
        vramRemoteLastKnownFreeMb: 7019, vramRemoteLastKnownNodes: 1,
        vramRemoteLastKnownAgeSec: 120,
        loadedModels: [],
    })]);
    assert.strictEqual(html.indexOf('последний замер'), -1,
        'история показана при выключенной раскладке — это выдуманная ёмкость');
    assert.strictEqual(html.indexOf('RPC-узлы'), -1, 'при выключенной раскладке RPC-строк быть не должно');
});

// --- 4. История нулевая — ничего не рисуем ----------------------------------
check('История нулевая: секция RPC не появляется', function () {
    const html = renderBackendsPage([cppBackend({
        totalVramMb: 8191, availableVramMb: 7073,
        rpcEnabled: true,
        vramRemoteLastKnownFreeMb: 0, vramRemoteLastKnownNodes: 0,
        vramRemoteLastKnownAgeSec: 0,
        loadedModels: [],
    })]);
    assert.strictEqual(html.indexOf('последний замер'), -1, 'нулевая история выдана за данные');
});

// --- 5. Возраст: часы для старых замеров ------------------------------------
check('Возраст > 1 часа показывается в часах', function () {
    const html = renderBackendsPage([cppBackend({
        totalVramMb: 8191, availableVramMb: 7073,
        rpcEnabled: true,
        vramRemoteLastKnownFreeMb: 7000, vramRemoteLastKnownNodes: 1,
        vramRemoteLastKnownAgeSec: 7200,
        loadedModels: [],
    })]);
    assert.ok(html.indexOf('назад 2 ч') !== -1, 'нет возраста в часах: ' + html.slice(0, 1600));
});

console.log('\nR348: ' + passed + ' проверок пройдено');
