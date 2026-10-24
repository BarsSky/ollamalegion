// gguf-model-params-bool-r134.test.js — R134 (доп., 2026-10-16).
//
// Запуск: node webui/js/modules/gguf-model-params-bool-r134.test.js
//
// ЖИВОЙ ДЕФЕКТ, который фиксирует этот тест. В форме параметров модели булевы
// поля были ГАЛОЧКАМИ, а collectModelParams записывал `!!checked` ВСЕГДА. Значит
// «Применить и перезагрузить», не тронув ни одной галочки, клал в профиль
// flashAttn=false, useMmap=false и enableReasoning=false. Это ровно то, на что
// жаловался оператор: «у загруженной модели стоит, что размышление включено, а в
// карточке оно выключено» — а после одного применения оно выключалось уже в
// профиле, и расхождение становилось настоящим.
//
// Проверяется:
//   1. булевы поля — ТРЁХЗНАЧНЫЕ (наследуется / включено / выключено), не галочки;
//   2. пустое значение = «наследуется» и в профиль НЕ попадает;
//   3. явные «включено»/«выключено» доезжают как true/false;
//   4. числовые поля: пустое не фиксируется, заполненное уходит числом.

'use strict';

const assert = require('assert');

global.window = global;
global.Utils = {
    escapeHtml: function (s) {
        return String(s == null ? '' : s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    }
};

const DICT = {
    'gguf.ctx_size': 'Размер контекста (суммарный)',
    'gguf.context_per_seq': 'Контекст на клиента',
    'gguf.gpu_layers': 'Слои GPU',
    'gguf.batch_size': 'Размер батча',
    'gguf.parallel': 'Параллельные слоты',
    'gguf.kv_cache_type': 'KV-cache quantization',
    'gguf.flash_attn': 'Flash Attention',
    'gguf.use_mmap': 'mmap (memory-mapped weights)',
    'gguf.reasoning': 'Размышления',
    'gguf.override_tensors': 'Override tensors (MoE)',
    'gguf.override_bufts': 'Override bufts (MoE)',
    'gguf.mp_table_active': 'Действующие значения',
    'gguf.mp_table_set': 'Задать для загрузки / перезагрузки',
    'gguf.mp_col_param': 'Параметр',
    'gguf.mp_col_value': 'Значение',
    'gguf.mp_col_source': 'Источник',
    'gguf.mp_col_profile': 'В профиле',
    'gguf.mp_col_set': 'Задать',
    'gguf.mp_inherit': 'наследуется',
    'gguf.mp_on': 'включено',
    'gguf.mp_off': 'выключено',
    'gguf.mp_unknown': '—',
    'gguf.mp_not_in_profile': '— не задано —',
    'gguf.mp_apply': 'Применить и перезагрузить',
    'gguf.mp_apply_hint': 'Применение сохранит профиль модели и перезагрузит её.',
    'gguf.mp_set_hint': 'Пустое поле = наследуется.',
    'gguf.mp_active_hint_loaded': 'Что реально действует у загруженной модели.',
    'gguf.mp_active_hint_disk': 'Модель не загружена.',
    'gguf.mp_advanced': 'Дополнительно',
    'gguf.mp_list_placeholder': 'через запятую'
};
global.I18N = {
    t: function (key) { return Object.prototype.hasOwnProperty.call(DICT, key) ? DICT[key] : key; }
};
global.window.I18N = global.I18N;

// Поля формы, которые «найдёт» collectModelParams: три булевых (наследуется /
// включено / выключено) и два числовых (пустое и заполненное).
function field(attrs, value) {
    return {
        value: value,
        type: 'select-one',
        tagName: 'SELECT',
        checked: false,
        getAttribute: function (k) { return attrs[k] === undefined ? null : attrs[k]; }
    };
}
const FIELDS = [
    field({ 'data-field': 'flashAttn', 'data-type': 'bool' }, ''),
    field({ 'data-field': 'enableReasoning', 'data-type': 'bool' }, 'true'),
    field({ 'data-field': 'useMmap', 'data-type': 'bool' }, 'false'),
    field({ 'data-field': 'contextLength', 'data-type': 'int' }, ''),
    field({ 'data-field': 'parallel', 'data-type': 'int' }, '2')
];
global.document = {
    querySelector: function () {
        return { querySelectorAll: function () { return FIELDS; } };
    },
    querySelectorAll: function () { return []; },
    getElementById: function () { return null; },
    createElement: function () { return { style: {}, setAttribute() {}, appendChild() {} }; },
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

let checks = 0;
function check(name, fn) { fn(); checks++; console.log('  ✓ ' + name); }

check('1: булевы поля формы — трёхзначный выбор, а не галочки', function () {
    const html = M.renderModelParamsEditor('m1', {}, { source: 'disk' });
    assert.ok(/data-type="bool"/.test(html), 'в форме нет трёхзначных bool-полей: ' + html.slice(0, 400));
    assert.ok(/<option value=""[^>]*>/.test(html), 'нет состояния «наследуется»');
    assert.ok(/<option value="true"/.test(html) && /<option value="false"/.test(html),
        'нет явных состояний «включено»/«выключено»');
    assert.ok(!/type="checkbox"[^>]*data-field="enableReasoning"/.test(html),
        'размышления снова рисуются галочкой: «не тронул» превратится в «выключено»');
});

check('2: пустое булево НЕ попадает в профиль (главное свойство)', function () {
    const body = M.collectModelParams('m1');
    assert.ok(body !== null, 'форма не найдена');
    assert.ok(!Object.prototype.hasOwnProperty.call(body, 'flashAttn'),
        'пустое (наследуется) уехало в профиль как ' + JSON.stringify(body.flashAttn) +
        ' — именно так применение формы молча выключало параметр, которого оператор не касался');
    assert.strictEqual(Object.prototype.hasOwnProperty.call(body, 'useMmap'), true,
        'явное «выключено» обязано доезжать до профиля');
    assert.strictEqual(body.useMmap, false, 'явное «выключено» должно быть false');
});

check('3: явные состояния доезжают как true/false', function () {
    const body = M.collectModelParams('m1');
    assert.strictEqual(body.enableReasoning, true,
        'выбранное «включено» для размышлений не доехало: ' + JSON.stringify(body));
});

check('4: числовые поля — пустое не фиксируется, заполненное уходит числом', function () {
    const body = M.collectModelParams('m1');
    assert.ok(!Object.prototype.hasOwnProperty.call(body, 'contextLength'),
        'пустое числовое поле уехало в профиль (сломало бы наследование)');
    assert.strictEqual(body.parallel, 2, 'заполненное поле должно быть числом');
});

console.log('\ngguf-model-params-bool-r134: ' + checks + ' проверок пройдено');
