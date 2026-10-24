// gguf-loaded-chips-r162.test.js — R162 (2026-10-17): чипы карточки загруженной
// модели показывают ФАКТ инстанса, а не устаревший runtime-документ, и называют
// расхождение с намерением оператора.
//
// ЖИВЫЕ ДЕФЕКТЫ, которые закрывает тест (нашёл инструмент, дошедший до чипов):
//   1. Воркер сообщал инстанс с окном 8192 и 38 слоями на GPU, а чип показывал
//      32768 и 19/42: предпочтение отдавалось runtime-документу (state.runtimeModels),
//      который к тому моменту устарел. Оператор видел числа, которых в инстансе нет.
//   2. R155 («окно больше профиля») не мог сработать ВООБЩЕ: намерение оператора
//      бралось из m.parameters.contextLength.value, а воркер это поле не отдаёт —
//      сравнивать было не с чем. Теперь намерение берётся из state.modelProfiles.
//
// КОНТРФАКТУАЛЬНОСТЬ: первый блок падает на старом коде (чип показал бы 32768);
// второй падает на старом коде (стрелки бы не было при любых данных).
//
// Запуск: node webui/js/modules/gguf-loaded-chips-r162.test.js
'use strict';

const assert = require('assert');

global.window = global;
global.Utils = {
    escapeHtml: function (s) {
        return String(s === null || s === undefined ? '' : s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    },
};
global.I18N = { t: function (key) { return key; } };
global.formatFileSize = function (n) { return String(n) + ' B'; };

function freshState(over) {
    return Object.assign({
        selectedBackendId: 'bk-1',
        registeredBackends: [{ id: 'bk-1', backendType: 'llama_cpp' }],
        detailPane: 'loaded',
        detailLoading: false,
        detailError: null,
        localModels: [],
        loadedModels: [],
        runtimeModels: {},
        modelProfiles: {},
        rpcLayout: null,
        workerInfo: null,
        gpuInfo: null,
        diskInventory: {},
        loadingModels: {},
        unloadingModels: {},
    }, over || {});
}

// МОДУЛЬ ГРУЗИМ ОДИН РАЗ и дальше меняем ОДИН И ТОТ ЖЕ объект состояния.
// Рендерер захватывает state при инициализации; подмена объекта (как в первой
// версии этого теста) не доезжает до кода — тесты начинают проверять чужое
// состояние и падают на собственной изоляции, а не на поведении продукта.
let MODULE = null;
const STATE = freshState();

function resetState(over) {
    Object.keys(STATE).forEach(function (k) { delete STATE[k]; });
    Object.assign(STATE, freshState(over));
    return STATE;
}

function loadModule() {
    if (MODULE) return MODULE;
    resetState({});
    global.GgufModule = {
        state: STATE,
        _: function (key, fallback) { return fallback || key; },
    };
    require('./gguf-renderer-detail-render.js');
    MODULE = global.GgufModule;
    assert.ok(typeof MODULE.renderLoadedPane === 'function', 'renderLoadedPane не экспортирован');
    return MODULE;
}

const results = [];

function check(name, fn) {
    try {
        fn();
        results.push({ ok: true, name: name });
    } catch (e) {
        results.push({ ok: false, name: name, err: e });
    }
}

function paneFor(over) {
    const M = loadModule();
    resetState(over);
    return M.renderLoadedPane();
}

function chipText(pane, label) {
    const re = new RegExp('>' + label + '=([^<]*)<');
    const m = pane.match(re);
    return m ? m[1] : null;
}

function chipTitle(pane, label) {
    // Подсказка идёт в title у того же span.
    const re = new RegExp('title="([^"]*)"[^>]*>' + label + '=');
    const m = pane.match(re);
    return m ? m[1] : null;
}

// ── 1. ФАКТ главнее устаревшего runtime-документа ───────────────────────────
check('факт инстанса (8192/38) побеждает устаревший runtime (32768/19)', function () {
    const pane = paneFor(freshState({
        loadedModels: [{
            name: 'gemma-4-E4B-it-Q4_K_M',
            path: '/app/models/gemma-4-E4B-it-Q4_K_M.gguf',
            context_size: 8192,
            context_per_seq: 4096,
            gpu_layers: 38,
            n_layers: 42,
            kv_cache_type: 'q4_0',
        }],
        runtimeModels: {
            'gemma-4-E4B-it-Q4_K_M': { context_size: 32768, gpu_layers: 19, n_layers: 42 },
        },
    }));
    const ctx = chipText(pane, 'ctx');
    assert.ok(ctx && ctx.indexOf('8192') === 0, 'ctx должен показывать ФАКТ 8192, получено: ' + ctx);
    assert.ok(ctx.indexOf('runtime 32768') > 0, 'расхождение с runtime-документом должно быть НАЗВАНО, получено: ' + ctx);
    const layers = chipText(pane, 'gpu_layers');
    assert.ok(layers && layers.indexOf('38/42') === 0, 'слои: ожидали 38/42, получено: ' + layers);
});

// ── 2. R158: частичная выгрузка подписана словами ───────────────────────────
check('частичная выгрузка: 38/42 (в RAM 4) и объяснение в подсказке', function () {
    const pane = paneFor(freshState({
        loadedModels: [{
            name: 'gemma-4-E4B-it-Q4_K_M',
            context_size: 8192,
            gpu_layers: 38,
            n_layers: 42,
        }],
    }));
    const v = chipText(pane, 'gpu_layers');
    assert.strictEqual(v, '38/42 (в RAM 4)', 'получено: ' + v);
    const t = chipTitle(pane, 'gpu_layers');
    assert.ok(t && t.indexOf('Частичная выгрузка') === 0, 'подсказка должна объяснять частичную выгрузку');
});

// ── 3. R155: окно больше профиля — стрелка и честная причина ────────────────
check('R155: факт 22016 против профиля 8192 — стрелка и объяснение', function () {
    const pane = paneFor(freshState({
        loadedModels: [{
            name: 'gemma-4-E4B-it-Q4_K_M',
            context_size: 22016,
            context_per_seq: 11008,
            gpu_layers: 42,
            n_layers: 42,
        }],
        modelProfiles: {
            'gemma-4-E4B-it-Q4_K_M': { contextLength: 8192 },
        },
    }));
    const ctx = chipText(pane, 'ctx');
    assert.ok(ctx && ctx.indexOf('22016') === 0, 'ctx: ожидали факт 22016, получено: ' + ctx);
    assert.ok(ctx.indexOf('↑') > 0, 'R155: окно больше профиля — обязана быть стрелка, получено: ' + ctx);
    const t = chipTitle(pane, 'ctx');
    assert.ok(t && t.indexOf('22016') > 0 && t.indexOf('8192') > 0,
        'подсказка обязана называть оба числа (факт и профиль), получено: ' + t);
    assert.ok(t.indexOf('запрос') > 0 || t.indexOf('параметр') > 0,
        'подсказка обязана называть причину, получено: ' + t);
});

// ── 4. Нет расхождения — нет стрелки и нет домыслов ─────────────────────────
check('факт равен профилю: ни стрелки, ни пометки runtime', function () {
    const pane = paneFor(freshState({
        loadedModels: [{
            name: 'gemma-4-E4B-it-Q4_K_M',
            context_size: 8192,
            context_per_seq: 4096,
            gpu_layers: 42,
            n_layers: 42,
        }],
        modelProfiles: { 'gemma-4-E4B-it-Q4_K_M': { contextLength: 8192 } },
    }));
    const ctx = chipText(pane, 'ctx');
    assert.strictEqual(ctx, '8192 (4096/slot)', 'получено: ' + ctx);
});

// ── 5. Намерения нет вовсе — стрелки быть не должно (не выдумываем) ─────────
check('нет профиля и нет параметров — стрелки нет', function () {
    const pane = paneFor(freshState({
        loadedModels: [{ name: 'm', context_size: 65536, gpu_layers: 42, n_layers: 42 }],
    }));
    const ctx = chipText(pane, 'ctx');
    assert.ok(ctx && ctx.indexOf('↑') < 0, 'без намерения стрелка была бы домыслом, получено: ' + ctx);
});

// ── 6. Раскладка по устройствам главнее поля gpu_layers ─────────────────────
// ЖИВОЙ СЛУЧАЙ 2026-10-17: чип показывал «0/42 — модель целиком на CPU», хотя
// воркер сообщал device_layers_exact с 42 слоями на CUDA0. Ложное предупреждение
// отправляет оператора искать несуществующую проблему.
check('device_layers_exact главнее gpu_layers=0 (не пугать «целиком на CPU»)', function () {
    const pane = paneFor(freshState({
        loadedModels: [{
            name: 'gemma-4-E4B-it-Q4_K_M',
            context_size: 8192,
            gpu_layers: 0,
            n_layers: 42,
            device_layers_exact: [
                { device: 'CPU', from: 0, to: 0, count: 1 },
                { device: 'CUDA0', from: 1, to: 42, count: 42 },
            ],
        }],
    }));
    const v = chipText(pane, 'gpu_layers');
    assert.strictEqual(v, '42/42', 'ожидали 42/42 из device_layers_exact, получено: ' + v);
    const t = chipTitle(pane, 'gpu_layers');
    assert.ok(t && t.indexOf('CPU') < 0, 'не должно быть предупреждения «целиком на CPU»');
});

check('R182: layout_degraded=true даёт чип «раскладка=НЕ применена»', function () {
    const pane = paneFor({
        loadedModels: [{
            name: 'gemma-4-E4B-it-Q4_K_M',
            state: 'loaded',
            layout_degraded: true,
            layout_degraded_reason: 'узлы 192.0.2.10:50052 переданы в мост, но ни один не стал устройством модели',
            device_layers_exact: [{ device: 'CUDA0', count: 42 }, { device: 'CPU', count: 1 }],
        }],
    });
    const v = chipText(pane, 'раскладка');
    assert.strictEqual(v, 'НЕ применена', 'чип должен назвать деградацию, получено: ' + JSON.stringify(v));
    const title = chipTitle(pane, 'раскладка');
    assert.ok(title && title.indexOf('192.0.2.10') >= 0,
        'в подсказке должна быть ПРИЧИНА с адресом узла, получено: ' + JSON.stringify(title));
});

check('R182: без деградации чипа «раскладка» нет', function () {
    const pane = paneFor({
        loadedModels: [{
            name: 'gemma-4-E4B-it-Q4_K_M',
            state: 'loaded',
            device_layers_exact: [{ device: 'CUDA0', count: 42 }, { device: 'CPU', count: 1 }],
        }],
    });
    assert.strictEqual(chipText(pane, 'раскладка'), null, 'ложное предупреждение о деградации недопустимо');
});

let failed = 0;
console.log('');
results.forEach(function (r) {
    console.log((r.ok ? '  PASS  ' : '  FAIL  ') + r.name + (r.ok ? '' : '\n        ' + r.err.message));
    if (!r.ok) failed++;
});
console.log('\n' + (failed === 0 ? 'RESULT: PASS' : 'RESULT: FAIL (' + failed + ')'));
process.exit(failed === 0 ? 0 : 1);
