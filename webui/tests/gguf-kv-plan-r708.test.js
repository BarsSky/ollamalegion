// gguf-kv-plan-r708.test.js — R708 (2026-11-23): раскладка KV в карточке
// модели на диске (страница GGUF → «Модели на диске»).
//
// ЗАЧЕМ. R707 считает KV послойно и публикует раскладку в /api/models, R708 —
// ещё и в /api/models/files (для НЕзагруженных моделей). Карточка модели на
// диске показывает размер и квантизацию; без строки раскладки оператор видит
// «24 слоя» рядом с 42 блоками и не может понять, почему KV на 32768 токенов
// 532 MiB, а не 3 GiB по формуле «все слои × одна ширина».
//
// ЧТО ПРОВЕРЯЕТСЯ (все числа — с живого стенда, gemma-4-E4B-it-Q4_K_M):
//   1. строка рисуется из полей плана и числа совпадают с воркером
//      (4 глобальных × 512 + 20 SWA × 256, окно 512, ~57 344 Б/токен по f16);
//   2. у модели без полей (старый воркер) строки НЕТ вообще — не «—»;
//   3. flat-план (плоская формула, раскладки нет) — строки тоже нет;
//   4. неполные поля (нет ширины/голов) — строки нет: не досчитываем «на глазок»;
//   5. нормализация ответа /api/models/files доносит поля до карточки;
//   6. i18n: ключи есть в ru и en, тексты в обеих локалях собираются, число
//      слоёв склоняется (24 слоя / 8 слоёв), «окно» появляется только с SWA;
//   7. подсказка объясняет ПОЧЕМУ не «все слои × ширина» и что Б/токен — по f16.
//
// Запуск: node webui/tests/gguf-kv-plan-r708.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

// Локаль выбирается ДО загрузки модулей: `_` внутри гуфф-модулей — это M._
// на момент загрузки (gguf-renderer-detail-render.js:54), подмена после
// загрузки ни на что не влияет. Поэтому en проверяется дочерним процессом.
const LANG = process.argv.indexOf('--lang=en') >= 0 ? 'en' : 'ru';

let checks = 0;
const failures = [];
function check(name, fn) {
    try {
        fn();
        checks++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' — ' + (e && e.message));
    }
}

// --- стенд модулей ---------------------------------------------------------
global.window = global;
global.Utils = {
    escapeHtml: function (s) {
        return String(s === null || s === undefined ? '' : s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }
};

// Настоящие словари (не заглушки): тест обязан проверить ТЕКСТ строки, а не
// только факт вызова перевода. Пакеты — обычные window.I18N_XX.
require('../js/i18n/ru.js');
require('../js/i18n/en.js');

function makeTranslator(pack) {
    return function (key, vars) {
        if (typeof vars === 'string') return vars; // fallback из M._()
        let s = pack[key];
        if (s === undefined || s === null) s = key;
        if (vars && typeof vars === 'object') {
            Object.keys(vars).forEach(function (p) {
                s = s.split('{' + p + '}').join(String(vars[p]));
            });
        }
        return s;
    };
}

global.I18N = {
    t: function (key) { return key; },
    getLang: function () { return 'ru'; }
};

global.GgufModule = {
    state: {
        selectedBackendId: 'bk-1',
        registeredBackends: [{ id: 'bk-1', backendType: 'llama_cpp' }],
        detailPane: 'models',
        detailLoading: false,
        detailError: null,
        localModels: [],
        loadedModels: [],
        runtimeModels: {},
        rpcLayout: null,
        workerInfo: null,
        gpuInfo: null,
        diskInventory: {},
        modelProfiles: {}
    },
    _: makeTranslator(LANG === 'en' ? global.I18N_EN : global.I18N_RU),
    stripGGUF: function (n) { return String(n || '').replace(/\.gguf$/i, ''); },
    formatFileSize: function (n) { return String(n) + ' B'; },
    formatVramMB: function (n) { return String(n); },
    showToast: function () {},
    refreshDetailPanel: function () {},
    refreshDetailPane: function () {}
};
global.GgufApi = {
    getRuntimeConfigViaBackend: function () { return Promise.resolve({ loaded_models: [] }); },
    listLocalModelsViaBackend: function () { return Promise.resolve({ files: [] }); },
    requestViaBackend: function () { return Promise.resolve({ files: [] }); }
};

require('../js/modules/gguf-renderer-refresh.js');
require('../js/modules/gguf-detail-models.js');
require('../js/modules/gguf-detail-params.js');
require('../js/modules/gguf-detail-settings.js');
require('../js/modules/gguf-renderer-detail-render.js');
const M = global.GgufModule;

// --- живые данные стенда (сняты `-feasible` с D:\ollama-legion-models) -----
// gemma-4-E4B-it-Q4_K_M: 4 глобальных слоя × (512+512) × 2 kv-heads +
// 20 SWA × (256+256) × 2 kv-heads, окно 512. f16: 16384 + 40960 = 57344 Б/токен.
const GEMMA_ROW = {
    name: 'gemma-4-E4B-it-Q4_K_M.gguf',
    size: 4215695776,
    quantization: 'Q4_K_M',
    state: 'available',
    kv_plan_source: 'plan',
    kv_plan_exact: true,
    kv_layers: 24,
    kv_global_layers: 4,
    kv_swa_layers: 20,
    kv_swa_window: 512,
    head_dim_k: 512,
    head_dim_v: 512,
    n_kv_heads: 2,
    head_dim_swa: 256,
    head_dim_swa_v: 256,
    n_kv_heads_swa: 2
};
// Qwen3.8-9B-Distill: 8 attention-слоёв × (256+256) × 4 kv-heads, окна нет.
const QWEN_ROW = {
    name: 'Qwen3.8-9B-Distill-uncensored-heretic.Q4_K_M.gguf',
    size: 5780091232,
    quantization: 'Q4_K_M',
    state: 'available',
    kv_plan_source: 'plan',
    kv_plan_exact: true,
    kv_layers: 8,
    kv_global_layers: 8,
    kv_swa_layers: 0,
    kv_swa_window: 0,
    head_dim_k: 256,
    head_dim_v: 256,
    n_kv_heads: 4
};
// Qwen3-4B: arch qwen3 не в списке правил раскладки → плоская формула.
const FLAT_ROW = {
    name: 'Qwen3-4B-Instruct-2507-Q4_K_M.gguf',
    size: 2497281120,
    quantization: 'Q4_K_M',
    state: 'available',
    kv_plan_source: 'flat',
    kv_plan_exact: false,
    kv_layers: 36,
    kv_global_layers: 36,
    kv_swa_layers: 0,
    head_dim_k: 128,
    head_dim_v: 128,
    n_kv_heads: 8
};
// «Старый воркер»: полей раскладки нет вовсе.
const OLD_ROW = { name: 'old-worker-model.gguf', size: 123456, quantization: '', state: 'available' };

// --- English: тот же путь перевода, другая локаль --------------------------
if (LANG === 'en') {
    const line = M.kvPlanLineText(M.kvPlanFacts(GEMMA_ROW));
    assert.strictEqual(line,
        'KV: 24 layers (4 global × 512, 20 SWA × 256, window 512) · from plan · ~57 344 B/token');
    assert.ok(M.kvPlanLineText(M.kvPlanFacts(Object.assign({}, QWEN_ROW, { kv_layers: 1, kv_global_layers: 1 })))
        .indexOf('1 layer ') >= 0, 'единственное число в en');
    const title = M.kvPlanLineTitle(M.kvPlanFacts(GEMMA_ROW));
    assert.ok(title.indexOf('all layers × one width') >= 0, 'en-подсказка не объясняет «все слои»: ' + title);
    assert.ok(title.indexOf('28') < 0 && title.indexOf('f16') >= 0, 'en-подсказка без f16');
    console.log('OK (en): ' + line);
    process.exit(0);
}

console.log('=== R708: раскладка KV в карточке модели на диске ===\n');

// --- 1. факты и текст строки ----------------------------------------------
check('факты gemma-4: 24 слоя (4 глобальных × 512 + 20 SWA × 256, окно 512), 57 344 Б/токен', function () {
    const f = M.kvPlanFacts(GEMMA_ROW);
    assert.ok(f, 'kvPlanFacts вернул null для строки с раскладкой');
    assert.strictEqual(f.layers, 24);
    assert.strictEqual(f.globalLayers, 4);
    assert.strictEqual(f.swaLayers, 20);
    assert.strictEqual(f.window, 512);
    assert.strictEqual(f.globalDim, 512);
    assert.strictEqual(f.swaDim, 256);
    assert.strictEqual(f.exact, true);
    assert.strictEqual(f.perTokenBytes, 57344,
        'Б/токен по f16: (512+512)·2·4 + (256+256)·2·20 = 57344');
});

check('текст строки совпадает с ожидаемым (ru)', function () {
    const text = M.kvPlanLineText(M.kvPlanFacts(GEMMA_ROW));
    assert.strictEqual(text,
        'KV: 24 слоя (4 глобальных × 512, 20 SWA × 256, окно 512) · по плану · ~57 344 Б/токен');
});

check('без окна — без слова «окно» (Qwen3.8-9B)', function () {
    const f = M.kvPlanFacts(QWEN_ROW);
    assert.ok(f);
    assert.strictEqual(f.perTokenBytes, 32768, '(256+256)·4·8·2 = 32768');
    assert.strictEqual(M.kvPlanLineText(f),
        'KV: 8 слоёв (8 глобальных × 256) · по плану · ~32 768 Б/токен');
});

check('верхняя оценка не выдаётся за точную', function () {
    const upper = Object.assign({}, QWEN_ROW, { kv_plan_exact: false });
    const text = M.kvPlanLineText(M.kvPlanFacts(upper));
    assert.ok(text.indexOf('верхняя оценка') >= 0, 'текст: ' + text);
    assert.ok(M.kvPlanLineTitle(M.kvPlanFacts(upper)).indexOf('верхняя оценка') >= 0,
        'подсказка должна называть оценку оценкой');
});

// --- 2. когда строки НЕТ ---------------------------------------------------
check('нет полей (старый воркер) — строки нет, а не «—»', function () {
    assert.strictEqual(M.kvPlanFacts(OLD_ROW), null);
    assert.strictEqual(M.renderKVPlanLine(OLD_ROW), '');
});

check('flat-план (плоская формула) — строки нет: раскладки нет', function () {
    assert.strictEqual(M.kvPlanFacts(FLAT_ROW), null);
    assert.strictEqual(M.renderKVPlanLine(FLAT_ROW), '');
});

check('неполные поля (нет ширины/голов) — строки нет: не досчитываем', function () {
    const noWidth = Object.assign({}, GEMMA_ROW);
    delete noWidth.head_dim_k;
    delete noWidth.head_dim_v;
    assert.strictEqual(M.kvPlanFacts(noWidth), null);
    const noHeads = Object.assign({}, QWEN_ROW);
    delete noHeads.n_kv_heads;
    assert.strictEqual(M.kvPlanFacts(noHeads), null);
});

// --- 3. HTML строки -------------------------------------------------------
check('HTML строки: класс, экранированная подсказка, текст', function () {
    const html = M.renderKVPlanLine(GEMMA_ROW);
    assert.ok(html.indexOf('class="gguf-local-kv"') >= 0, html);
    assert.ok(html.indexOf('title="') >= 0, 'нет подсказки');
    assert.ok(html.indexOf('57 344') >= 0, 'нет числа Б/токен');
    assert.ok(html.indexOf('<span') === 0 && html.indexOf('</span>') > 0, html);
    // Подсказка объясняет, почему не «все слои × ширина» и что число — по f16.
    const title = M.kvPlanLineTitle(M.kvPlanFacts(GEMMA_ROW));
    assert.ok(title.indexOf('все слои модели × одна ширина') >= 0, title);
    assert.ok(title.indexOf('f16') >= 0, title);
    assert.ok(title.indexOf('окно 512') >= 0 || title.indexOf('окне') >= 0 || title.indexOf('{window}') < 0,
        'окно не подставлено в подсказку: ' + title);
});

// --- 4. карточка списка «Модели на диске» ---------------------------------
function cardFor(html, name) {
    const idx = html.indexOf(name);
    assert.ok(idx >= 0, 'в панели нет карточки ' + name);
    const start = html.lastIndexOf('<div class="gguf-local-card">', idx);
    const end = html.indexOf('</div></div>', idx);
    return html.slice(start < 0 ? 0 : start, end < 0 ? html.length : end);
}

check('renderModelsPane: у gemma-4 строка есть, у модели без плана — нет', function () {
    M.state.localModels = [
        Object.assign({}, GEMMA_ROW),
        Object.assign({}, QWEN_ROW),
        Object.assign({}, FLAT_ROW),
        Object.assign({}, OLD_ROW)
    ];
    M.state.loadedModels = [];
    M.state.diskInventory = { 'bk-1': { count: 4, scannedAt: '' } };

    const html = M.renderModelsPane();
    const gemma = cardFor(html, GEMMA_ROW.name);
    assert.ok(gemma.indexOf('KV: 24 слоя') >= 0, 'у gemma-4 нет строки раскладки: ' + gemma);
    const qwen = cardFor(html, QWEN_ROW.name);
    assert.ok(qwen.indexOf('KV: 8 слоёв') >= 0, 'у Qwen3.8-9B нет строки раскладки: ' + qwen);
    const flat = cardFor(html, FLAT_ROW.name);
    assert.ok(flat.indexOf('gguf-local-kv') < 0, 'у flat-модели строка раскладки не должна рисоваться');
    const old = cardFor(html, OLD_ROW.name);
    assert.ok(old.indexOf('gguf-local-kv') < 0, 'у модели старого воркера строки быть не должно');
    assert.ok(old.indexOf('KV:') < 0, 'у модели старого воркера не должно быть даже «KV:»');
    // Размер и квантизация рядом не потерялись (вёрстка не переделана).
    assert.ok(gemma.indexOf('4 215 695 776 B') >= 0 || gemma.indexOf('4215695776 B') >= 0,
        'пропал размер модели: ' + gemma);
    assert.ok(gemma.indexOf('Q4_K_M') >= 0, 'пропала квантизация: ' + gemma);
});

// --- 5. нормализация ответа воркера ---------------------------------------
check('normalizeDiskModelRow доносит поля раскладки из /api/models/files', function () {
    const row = M.normalizeDiskModelRow(Object.assign({}, GEMMA_ROW, { sizeBytes: 4215695776 }));
    assert.strictEqual(row.name, GEMMA_ROW.name);
    assert.strictEqual(row.state, 'available');
    assert.strictEqual(row.kv_plan_source, 'plan');
    assert.strictEqual(row.kv_layers, 24);
    assert.strictEqual(row.head_dim_k, 512);
    assert.strictEqual(row.n_kv_heads_swa, 2);
});

check('normalizeDiskModelRow не выдумывает отсутствующие поля', function () {
    const row = M.normalizeDiskModelRow({ name: 'x.gguf', size: 1 });
    assert.strictEqual(row.quantization, '');
    Object.keys(row).forEach(function (k) {
        assert.ok(k !== 'kv_plan_source' && k !== 'kv_layers' && k !== 'head_dim_k',
            'появилось выдуманное поле ' + k);
    });
});

// --- 6. i18n --------------------------------------------------------------
const USED_KEYS = [
    'gguf.kv_plan.line', 'gguf.kv_plan.line_window',
    'gguf.kv_plan.global_part', 'gguf.kv_plan.swa_part',
    'gguf.kv_plan.layer_one', 'gguf.kv_plan.layer_few', 'gguf.kv_plan.layer_many',
    'gguf.kv_plan.source_plan', 'gguf.kv_plan.source_plan_upper',
    'gguf.kv_plan.title', 'gguf.kv_plan.title_window', 'gguf.kv_plan.title_upper_note'
];
check('все ключи строки есть в ru и en (непустые, не равны ключу)', function () {
    USED_KEYS.forEach(function (key) {
        ['I18N_RU', 'I18N_EN'].forEach(function (packName) {
            const v = global[packName][key];
            assert.ok(typeof v === 'string' && v.length > 0, packName + ': нет ключа ' + key);
            assert.ok(v !== key, packName + ': значение равно ключу ' + key);
        });
    });
});

check('словари симметричны по добавленным ключам', function () {
    const ru = global.I18N_RU;
    const en = global.I18N_EN;
    USED_KEYS.forEach(function (key) {
        assert.ok(ru[key] !== undefined && en[key] !== undefined, 'ключ только в одном пакете: ' + key);
    });
});

check('английская локаль собирает строку (layer/layers, B/token)', function () {
    // `_` в модуле — это M._ на момент загрузки, поэтому локаль English
    // проверяется в ДОЧЕРНЕМ процессе: подмена M._ после загрузки не влияет
    // (const _ = M._ в gguf-renderer-detail-render.js:54). Так проверяется
    // настоящий путь перевода, а не копия шаблона.
    const { execFileSync } = require('child_process');
    execFileSync(process.execPath, [__filename, '--lang=en'], { stdio: 'pipe', encoding: 'utf8' });
});

check('склонение числа слоёв (ru): 24 слоя / 48 слоёв / 1 слой', function () {
    const f24 = M.kvPlanFacts(Object.assign({}, GEMMA_ROW, { kv_layers: 24 }));
    assert.ok(M.kvPlanLineText(f24).indexOf('24 слоя') >= 0);
    const f48 = M.kvPlanFacts(Object.assign({}, GEMMA_ROW, { kv_layers: 48 }));
    assert.ok(M.kvPlanLineText(f48).indexOf('48 слоёв') >= 0, '48 слоёв: ' + M.kvPlanLineText(f48));
    const f1 = M.kvPlanFacts(Object.assign({}, GEMMA_ROW, { kv_layers: 1 }));
    assert.ok(M.kvPlanLineText(f1).indexOf('1 слой') >= 0, '1 слой: ' + M.kvPlanLineText(f1));
});

// --- 7. сторож: строка действительно вставляется в карточку ----------------
check('renderModelsPane вызывает renderKVPlanLine (строку снова не потерять)', function () {
    const src = fs.readFileSync(path.join(__dirname, '..', 'js', 'modules', 'gguf-detail-models.js'), 'utf8');
    assert.ok(/renderKVPlanLine\(m\)/.test(src),
        'в renderModelsPane нет вызова renderKVPlanLine(m) — строка не попадёт в карточку');
});

console.log('\n' + checks + ' проверок зелёные, ' + failures.length + ' красных');
if (failures.length > 0) {
    failures.forEach(function (f) { console.error('  FAIL: ' + f); });
    process.exit(1);
}
console.log('RESULT: PASS');
