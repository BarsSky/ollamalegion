// gguf-model-params-r134.test.js — R134 (2026-10-16).
//
// Запуск: node webui/js/modules/gguf-model-params-r134.test.js
//
// ЖИВЫЕ ЖАЛОБЫ ОПЕРАТОРА, которые закрывает этот тест:
//   1. «в карточке, где меняются параметры, потеряна локализация» — в интерфейсе
//      видны сырые ключи (gguf.parallel, gguf.reasoning) вместо подписей;
//   2. «рассинхрон с выставленными параметрами: у загруженной модели стоит, что
//      размышления включены, а в карточке галочка выключена»;
//   3. «описанных параметров больше, чем представлено в карточке»;
//   4. «параметры разбросаны по области — лучше два списка-таблицы: активные и
//      отдельно те, что можно задать; и одинаково для моделей на диске и
//      загруженных, в едином стиле».
//
// Проверяется:
//   A. реестр параметров совпадает с контрактом бэкенда (11 параметров загрузки);
//   B. в карточке ДВЕ таблицы: «действующие значения» и «задать для загрузки»;
//   C. факт берётся из полей ВОРКЕРА (reasoningEnabled), а не из выдуманного
//      m.enableReasoning — иначе галочка «размышления» не совпадала с моделью;
//   D. источник значения показывается (request/profile/default_profile/backend);
//   E. модель на диске и загруженная получают ОДИН набор параметров;
//   F. значения профиля раскладываются по форме (файл профиля → колонка
//      «В профиле» и поле ввода);
//   G. все подписи идут через словарь (_), то есть локализация не теряется.

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

// Словарь: ключ → подпись. Если код запросит ключ, которого нет, вернём САМ КЛЮЧ —
// и тест G поймает это как потерю локализации.
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
    'gguf.mp_ctx_hint': 'Окно модели в токенах.',
    'gguf.mp_ctx_seq_hint': 'Окно на одного клиента.',
    'gguf.mp_gpu_hint': '-1 = все слои на GPU.',
    'gguf.mp_batch_hint': 'Размер батча.',
    'gguf.mp_parallel_hint': 'Сколько запросов одновременно.',
    'gguf.mp_kv_hint': 'Тип KV-кэша.',
    'gguf.mp_flash_hint': 'Flash Attention.',
    'gguf.mp_mmap_hint': 'Отображение весов в память.',
    'gguf.mp_reasoning_hint': 'Размышления модели.',
    'gguf.mp_override_hint': 'Регэкспы тензоров.',
    'gguf.mp_override_bufts_hint': 'Куда положить тензоры.',
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
    'gguf.mp_active_hint_loaded': 'Что реально действует у загруженной модели и кто это поставил.',
    'gguf.mp_active_hint_disk': 'Модель не загружена — действующих значений нет.',
    'gguf.mp_advanced': 'Дополнительно: per-tensor override для MoE-моделей',
    'gguf.mp_list_placeholder': 'через запятую'
};
global.I18N = {
    t: function (key) { return Object.prototype.hasOwnProperty.call(DICT, key) ? DICT[key] : key; }
};
global.window.I18N = global.I18N;

// DOM-заглушка: модуль использует document только в hydrate/fill.
function makeEl(attrs) {
    const el = {
        _attrs: Object.assign({}, attrs || {}),
        innerHTML: '', value: '', checked: false, type: '',
        setAttribute(k, v) { this._attrs[k] = v; },
        getAttribute(k) { return this._attrs[k] === undefined ? null : this._attrs[k]; },
        removeAttribute(k) { delete this._attrs[k]; },
        querySelector() { return null; },
        querySelectorAll() { return []; },
        closest() { return null; }
    };
    return el;
}
global.document = {
    querySelector: function () { return null; },
    querySelectorAll: function () { return []; },
    getElementById: function () { return null; },
    createElement: function () { return makeEl(); },
    addEventListener: function () {}
};

const fs = require('fs');
const path = require('path');
const NodeModule = require('module');
const SRC = path.join(__dirname, 'gguf-renderer-detail-render.js');
// Модуль — IIFE, которая регистрируется в window.GgufModule (как в браузере).
// Переводчик `_` приходит из общей части страницы (const _ = M._), поэтому
// кладём его в GgufModule ДО загрузки модуля — ровно как это делает страница.
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

// Модель так, как её отдаёт ВОРКЕР: snake_case и reasoningEnabled (а не
// enableReasoning, которого воркер не отдаёт вовсе).
const LOADED_MODEL = {
    name: 'gemma-4-E4B-it-Q4_K_M',
    context_size: 65536,
    gpu_layers: -1,
    batch_size: 512,
    parallel: 2,
    context_per_seq: 32768,
    kv_cache_type: 'q4_0',
    flash_attn: true,
    use_mmap: true,
    reasoningEnabled: true,
    parameters: {
        contextLength: { value: 65536, source: 'profile' },
        gpuLayers: { value: -1, source: 'profile' },
        enableReasoning: { value: true, source: 'profile' },
        kvCacheType: { value: 'q4_0', source: 'default_profile' }
    }
};

check('A: реестр содержит все 11 параметров контракта загрузки', function () {
    const fields = M.MODEL_PARAM_SPECS.map(function (s) { return s.field; });
    ['contextLength', 'contextPerSeq', 'numGpuLayers', 'batchSize', 'parallel', 'kvCacheType',
     'flashAttn', 'useMmap', 'enableReasoning', 'overrideTensors', 'overrideTensorBufts']
        .forEach(function (f) {
            assert.ok(fields.indexOf(f) !== -1, 'в реестре нет параметра ' + f);
        });
    assert.strictEqual(fields.length, 11, 'в реестре лишние/недостающие параметры: ' + fields.join(','));
});

check('B: карточка содержит ДВЕ таблицы — действующие и «задать»', function () {
    const html = M.renderModelParamsEditor('gemma-4-E4B-it-Q4_K_M', {}, { source: 'loaded', model: LOADED_MODEL, runtime: {} });
    assert.ok(html.indexOf('gguf-mp-table') !== -1, 'нет таблиц параметров');
    assert.strictEqual((html.match(/class="gguf-mp-table"/g) || []).length, 2, 'таблиц должно быть ровно две');
    assert.ok(html.indexOf('Действующие значения') !== -1, 'нет заголовка «действующие значения»');
    assert.ok(html.indexOf('Задать для загрузки / перезагрузки') !== -1, 'нет заголовка «задать»');
    assert.ok(html.indexOf('>Параметр<') !== -1, 'нет колонки «Параметр»');
    assert.ok(html.indexOf('>Источник<') !== -1, 'нет колонки «Источник»');
});

check('C: «размышления» читаются из факта воркера (reasoningEnabled), а не из m.enableReasoning', function () {
    // Как в проде: значения формы — из loadedParamsFor (происхождение → факт).
    const values = M.loadedParamsFor(LOADED_MODEL, {});
    assert.strictEqual(values.enableReasoning, true,
        'loadedParamsFor не увидел включённые размышления (именно этот дефект показывал галочку выключенной)');
    const html = M.renderModelParamsEditor('gemma-4-E4B-it-Q4_K_M', values, { source: 'loaded', model: LOADED_MODEL, runtime: {} });
    // Активная таблица должна показать «включено» для строки размышлений.
    const reasoningRow = html.split('<tr>').filter(function (r) { return r.indexOf('Размышления') !== -1; })[0] || '';
    assert.ok(reasoningRow.indexOf('включено') !== -1,
        'активная таблица не показывает включённые размышления: ' + reasoningRow);
    // И поле в форме «задать» — тоже «включено». R134 (доп., 2026-10-16): это
    // ТРЁХЗНАЧНЫЙ выбор, а не галочка: «не тронуто» обязано остаться
    // «наследуется», иначе применение формы молча выключает параметр
    // (живой дефект: «в карточке выключено, хотя модель работает с включённым»).
    const field = html.split('data-field="enableReasoning"')[1] || '';
    assert.ok(field.indexOf('data-type="bool"') !== -1,
        'поле размышлений должно быть трёхзначным (наследуется/включено/выключено): ' + field.slice(0, 160));
    assert.ok(/<option value="true" selected>/.test(field),
        'в форме «задать» не выбрано «включено» при reasoningEnabled=true: ' + field.slice(0, 240));
    // А если воркер сказал false — показываем выключено (не выдумываем).
    const offValues = M.loadedParamsFor({ reasoning_enabled: false }, {});
    const off = M.renderModelParamsEditor('m', offValues, { source: 'loaded', model: { reasoning_enabled: false }, runtime: {} });
    const offRow = off.split('<tr>').filter(function (r) { return r.indexOf('Размышления') !== -1; })[0] || '';
    assert.ok(offRow.indexOf('выключено') !== -1, 'выключенные размышления показаны неверно: ' + offRow);
});

check('D: источник значения показан словами (профиль/по умолчанию)', function () {
    const html = M.renderModelParamsEditor('gemma-4-E4B-it-Q4_K_M', {}, { source: 'loaded', model: LOADED_MODEL, runtime: {} });
    assert.ok(html.indexOf('профиль') !== -1, 'источник «профиль» не показан');
    assert.ok(html.indexOf('по умолчанию') !== -1, 'источник «по умолчанию» не показан');
});

check('E: модель на диске и загруженная получают ОДИН набор параметров', function () {
    const loaded = M.renderModelParamsEditor('m', {}, { source: 'loaded', model: LOADED_MODEL, runtime: {} });
    const disk = M.renderModelParamsEditor('m', {}, { source: 'disk' });
    const fieldsOf = function (html) {
        return (html.match(/data-field="[a-zA-Z]+"/g) || []).map(function (s) { return s.slice(12, -1); }).sort();
    };
    assert.deepStrictEqual(fieldsOf(disk), fieldsOf(loaded), 'наборы параметров разошлись между вкладками');
    assert.ok(disk.indexOf('Модель не загружена') !== -1, 'на диске не сказано, что действующих значений нет');
});

check('F: значения профиля раскладываются по форме и по колонке «В профиле»', function () {
    const cells = {};
    const inputs = {};
    const editor = makeEl({ 'data-model': 'm' });
    editor.querySelector = function (sel) {
        const mm = /data-profile-field="([a-zA-Z]+)"/.exec(sel);
        if (mm) { cells[mm[1]] = cells[mm[1]] || makeEl({}); return cells[mm[1]]; }
        const fm = /data-field="([a-zA-Z]+)"/.exec(sel);
        if (fm) {
            if (!inputs[fm[1]]) {
                const spec = M.MODEL_PARAM_SPECS.filter(function (s) { return s.field === fm[1]; })[0] || {};
                inputs[fm[1]] = makeEl({});
                inputs[fm[1]].type = spec.type === 'bool' ? 'checkbox'
                    : (spec.type === 'enum' ? 'select-one' : 'text');
            }
            return inputs[fm[1]];
        }
        return null;
    };
    M.fillModelParamEditor(editor, { contextLength: 32768, numGpuLayers: -1, kvCacheType: 'q8_0', enableReasoning: true });
    assert.strictEqual(inputs.contextLength.value, '32768', 'контекст профиля не подставлен');
    assert.strictEqual(inputs.numGpuLayers.value, '-1', 'слои GPU профиля не подставлены');
    assert.strictEqual(inputs.kvCacheType.value, 'q8_0', 'KV-cache профиля не подставлен');
    assert.strictEqual(inputs.enableReasoning.checked, true, 'галочка размышлений профиля не отмечена');
    assert.strictEqual(inputs.parallel.value, '', 'незаданный параметр должен остаться пустым (наследование)');
    assert.ok(String(cells.contextLength.innerHTML).indexOf('32768') !== -1, 'колонка «в профиле» не заполнена');
    assert.ok(String(cells.parallel.innerHTML).indexOf('не задано') !== -1, 'пустой профиль должен быть «не задано»');
});

check('G: все подписи идут через словарь — сырых ключей i18n в карточке нет', function () {
    const html = M.renderModelParamsEditor('gemma-4-E4B-it-Q4_K_M', {}, { source: 'loaded', model: LOADED_MODEL, runtime: {} });
    const leftovers = (html.match(/gguf\.[a-z_]+/g) || []).filter(function (k) {
        return !Object.prototype.hasOwnProperty.call(DICT, k);
    });
    assert.deepStrictEqual(leftovers, [], 'в разметке остались ключи без перевода: ' + leftovers.join(', '));
    assert.ok(html.indexOf('>gguf.') === -1, 'в интерфейс попал сырой ключ как текст');
});

console.log('\ngguf-model-params-r134: ' + checks + ' проверок пройдено');
