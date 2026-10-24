// gguf-inherited-defaults-r229.test.js — R229 (2026-10-22).
//
// Запуск: node webui/js/modules/gguf-inherited-defaults-r229.test.js
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «в профиле пусто = наследуется», но из интерфейса не
// видно, ЧТО именно унаследуется. Пустая метка читалась как «выключено/ноль»,
// хотя фактически действуют настройки по умолчанию (default_profile) или env
// воркера — например kvCacheType=q8_0, numGpuLayers=30, contextLength=32768.
//
// Ответ у балансера уже есть: GET /api/v1/cppworker/load-defaults →
// { effective: {...}, source: {...} }. Теперь метка «наследуется» несёт это
// значение, а форма подписывается им сразу после загрузки профиля.
//
// Что проверяем:
//   1. inheritedValueOf — единое правило преобразования effective → значение
//      (int/enum как есть, flashAttn 1/-1 → да/нет, contextPerSlot → contextPerSeq,
//      отсутствующее/пустое → null);
//   2. inheritedLabel — текст «наследуется (значение)», а без данных — честное
//      «наследуется» без скобки;
//   3. paramInput показывает значение в placeholder (числа) и в опции-заглушке
//      (bool/enum);
//   4. renderModelParamsEditor прокидывает opts.defaults в поля;
//   5. applyInheritedLabels обновляет уже отрисованную форму (данные приходят
//      отдельным запросом после первой отрисовки).

'use strict';

const assert = require('assert');

global.window = global;
global.MonitorApp = { esc: function (s) { return String(s == null ? '' : s); } };
global.Utils = {
    escapeHtml: function (s) {
        return String(s == null ? '' : s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    }
};

const DICT = {
    'gguf.mp_inherit': 'наследуется',
    'gguf.mp_on': 'включено',
    'gguf.mp_off': 'выключено'
};
global.I18N = { t: function (key, fallback) { return Object.prototype.hasOwnProperty.call(DICT, key) ? DICT[key] : (fallback === undefined ? key : fallback); } };
global.window.I18N = global.I18N;

global.document = {
    querySelector: function () { return null; },
    querySelectorAll: function () { return []; },
    getElementById: function () { return null; },
    createElement: function () { return {}; },
    addEventListener: function () {}
};

const fs = require('fs');
const path = require('path');
const SRC = path.join(__dirname, 'gguf-renderer-detail-render.js');
global.window.GgufModule = {
    _: function (key, fallback) {
        if (Object.prototype.hasOwnProperty.call(DICT, key)) return DICT[key];
        return fallback === undefined ? key : fallback;
    }
};
require(SRC);
const M = global.window.GgufModule;

// Значения по умолчанию — как их отдаёт GET /api/v1/cppworker/load-defaults
// (живой ответ стенда, 2026-10-22).
const EFFECTIVE = {
    batchSize: 512,
    contextLength: 32768,
    contextPerSlot: 16384,
    enableReasoning: false,
    flashAttn: 1,
    kvCacheType: 'q8_0',
    numGpuLayers: 30,
    parallel: 2,
    useMmap: true
};

const specOf = function (field) {
    return M.MODEL_PARAM_SPECS.filter(function (s) { return s.field === field; })[0];
};

let checks = 0;
function check(name, fn) { fn(); checks++; console.log('  ✓ ' + name); }

check('R229: у каждого параметра есть ключ значений по умолчанию', function () {
    ['contextLength', 'contextPerSeq', 'numGpuLayers', 'batchSize', 'parallel',
        'kvCacheType', 'flashAttn', 'useMmap', 'enableReasoning'].forEach(function (f) {
        const spec = specOf(f);
        assert.ok(spec && spec.defaultKey, 'у ' + f + ' нет defaultKey — «наследуется» останется без значения');
        assert.ok(Object.prototype.hasOwnProperty.call(EFFECTIVE, spec.defaultKey),
            'defaultKey ' + (spec && spec.defaultKey) + ' отсутствует в живом ответе load-defaults');
    });
});

check('R229: inheritedValueOf преобразует effective в значение параметра', function () {
    assert.strictEqual(M.inheritedValueOf(specOf('contextLength'), EFFECTIVE), 32768);
    assert.strictEqual(M.inheritedValueOf(specOf('contextPerSeq'), EFFECTIVE), 16384,
        'окно на клиента берётся из contextPerSlot');
    assert.strictEqual(M.inheritedValueOf(specOf('numGpuLayers'), EFFECTIVE), 30);
    assert.strictEqual(M.inheritedValueOf(specOf('kvCacheType'), EFFECTIVE), 'q8_0');
    assert.strictEqual(M.inheritedValueOf(specOf('flashAttn'), EFFECTIVE), true,
        'flashAttn=1 из effective — это «включено»');
    assert.strictEqual(M.inheritedValueOf(specOf('useMmap'), EFFECTIVE), true);
    assert.strictEqual(M.inheritedValueOf(specOf('enableReasoning'), EFFECTIVE), false);
    // -1 в контракте загрузки значит «выключено», не «нет данных».
    assert.strictEqual(M.inheritedValueOf(specOf('flashAttn'), { flashAttn: -1 }), false);
    // Нет данных — нет значения (иначе показали бы выдуманное).
    assert.strictEqual(M.inheritedValueOf(specOf('contextLength'), {}), null);
    assert.strictEqual(M.inheritedValueOf(specOf('contextLength'), null), null);
    assert.strictEqual(M.inheritedValueOf(specOf('numGpuLayers'), { numGpuLayers: 0 }), 0,
        '0 — валидное значение (наследовать число слоёв нечем), его нельзя терять');
});

check('R229: inheritedLabel пишет значение, а без данных — просто «наследуется»', function () {
    assert.strictEqual(M.inheritedLabel(specOf('contextLength'), EFFECTIVE), 'наследуется (32768)');
    assert.strictEqual(M.inheritedLabel(specOf('kvCacheType'), EFFECTIVE), 'наследуется (q8_0)');
    assert.strictEqual(M.inheritedLabel(specOf('flashAttn'), EFFECTIVE), 'наследуется (включено)');
    assert.strictEqual(M.inheritedLabel(specOf('useMmap'), {}), 'наследуется');
});

check('R229: paramInput показывает унаследованное значение в поле', function () {
    const numberHtml = M.paramInput(specOf('contextLength'), 'idb', '', EFFECTIVE);
    assert.ok(/placeholder="наследуется \(32768\)"/.test(numberHtml), 'нет значения в placeholder: ' + numberHtml);

    const kvHtml = M.paramInput(specOf('kvCacheType'), 'idb', '', EFFECTIVE);
    assert.ok(/<option value="[^"]*"[^>]*>наследуется \(q8_0\)<\/option>/.test(kvHtml),
        'нет значения в опции «наследуется»: ' + kvHtml);

    const boolHtml = M.paramInput(specOf('flashAttn'), 'idb', '', EFFECTIVE);
    assert.ok(/наследуется \(включено\)/.test(boolHtml), 'нет значения у bool: ' + boolHtml);

    const noData = M.paramInput(specOf('contextLength'), 'idb', '', null);
    assert.ok(/placeholder="наследуется"/.test(noData), 'без данных метка должна быть без скобки: ' + noData);
});

check('R229: renderModelParamsEditor прокидывает opts.defaults в поля', function () {
    const html = M.renderModelParamsEditor('gemma-4-E4B-it-Q4_K_M', {}, {
        source: 'disk', defaults: { effective: EFFECTIVE }
    });
    assert.ok(/наследуется \(32768\)/.test(html), 'значение окна не прокинуто в форму');
    assert.ok(/наследуется \(q8_0\)/.test(html), 'значение KV не прокинуто в форму');
    assert.ok(/наследуется \(включено\)/.test(html), 'значение bool не прокинуто в форму');
});

check('R229: applyInheritedLabels обновляет уже отрисованную форму', function () {
    const opt = { textContent: 'наследуется' };
    const inputNumber = { placeholder: 'наследуется' };
    const inputKv = { options: [], querySelector: function () { return opt; } };
    const fields = {
        contextLength: inputNumber,
        kvCacheType: inputKv
    };
    const editor = {
        querySelector: function (sel) {
            const m = /data-field="([^"]+)"/.exec(sel);
            return m ? (fields[m[1]] || null) : null;
        }
    };
    M.applyInheritedLabels(editor, EFFECTIVE);
    assert.strictEqual(inputNumber.placeholder, 'наследуется (32768)', 'placeholder не обновлён');
    assert.strictEqual(opt.textContent, 'наследуется (q8_0)', 'опция «наследуется» не подписана значением');
});

console.log('\nOK: ' + checks + ' checks passed');
