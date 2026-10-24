// unverified-modes-hidden-r118.test.js — R118 (2026-10-14).
//
// Запуск: node webui/js/modules/unverified-modes-hidden-r118.test.js
//
// ТРЕБОВАНИЕ ОПЕРАТОРА: «не отображать в настройках те режимы, что не проверены».
//
// Что проверяется:
//   1. в настройках виден только ПРОВЕРЕННЫЙ режим работы (standard): карточки
//      replication / rpc_coordinator / virtual_router / distributed_inference
//      скрыты (display:none);
//   2. в форме загрузки нет непроверенных backend'ов раскладки (Vulkan/Kompute —
//      раскладка проверена живьём на CUDA) и непроверенных split-режимов
//      (ROW/NONE/TENSOR — раскладка слоёв работает в LAYER);
//   3. скрытие сделано так, чтобы не ломать разметку и сохранение настроек:
//      элементы остаются в DOM, а radio «standard» отмечен.

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

global.window = global;
global.Utils = { escapeHtml: function (s) { return String(s === null || s === undefined ? '' : s); } };
global.I18N = { t: function (key) { return key; } };
global.GgufModule = {
    state: { selectedBackendId: null, runtimeModels: {}, rpcLayout: null, detailPane: 'models' },
    _: function (key) { return key; },
    stripGGUF: function (n) { return n; },
    formatFileSize: function () { return '-'; },
    formatVramMB: function (n) { return String(n); },
    showToast: function () {},
    refreshDetailPane: function () {},
    refreshDetailPanel: function () {}
};
global.GgufApi = {
    getRuntimeConfigViaBackend: function () { return Promise.resolve({ loaded_models: [] }); },
    listLocalModelsViaBackend: function () { return Promise.resolve({ files: [] }); },
    requestViaBackend: function () { return Promise.resolve({}); }
};

require('./gguf-renderer-refresh.js');
require('./gguf-renderer-detail-render.js');
const M = global.GgufModule;

let checks = 0;
function check(name, fn) { fn(); checks++; console.log('  ✓ ' + name); }

const INDEX = path.join(__dirname, '..', '..', 'index.html');

check('R118: в настройках виден только проверенный режим standard', function () {
    const html = fs.readFileSync(INDEX, 'utf8');
    const cards = html.match(/<label class="mode-card"[^>]*data-mode="[a-z_]+"[^>]*>/g) || [];
    assert.ok(cards.length >= 5, 'не нашёл карточки режимов: ' + cards.length);

    const visible = [];
    cards.forEach(function (tag) {
        const mode = (tag.match(/data-mode="([a-z_]+)"/) || [])[1];
        if (!/display\s*:\s*none/.test(tag)) visible.push(mode);
    });
    assert.deepStrictEqual(visible, ['standard'],
        'видны непроверенные режимы работы: ' + visible.join(', '));

    // Radio «standard» отмечен по умолчанию — стандартный режим и есть общий.
    assert.ok(/name="operatingMode" value="standard" checked/.test(html),
        'radio standard должен быть отмечен по умолчанию');
    // Непроверенные radio остались в DOM (чтобы не ломать логику сохранения),
    // но их карточки скрыты — проверим, что каждый такой radio внутри скрытой.
    ['replication', 'rpc_coordinator', 'virtual_router', 'distributed_inference'].forEach(function (mode) {
        const re = new RegExp('<label class="mode-card"[^>]*data-mode="' + mode + '"[^>]*style="display:none"');
        assert.ok(re.test(html), 'карточка режима ' + mode + ' не скрыта');
    });
});

check('R118: в форме загрузки нет непроверенных backend раскладки', function () {
    const list = M.GGUF_VERIFIED_RPC_BACKENDS || [];
    const values = list.map(function (o) { return String(o.value); });
    assert.deepStrictEqual(values, ['', 'cuda'],
        'в списке backend раскладки остались непроверенные варианты: ' + values.join(', '));
    // И в исходнике формы их тоже нет (страховка от возврата опций мимо списка).
    const src = fs.readFileSync(path.join(__dirname, 'gguf-renderer-detail-render.js'), 'utf8');
    ['vulkan', 'kompute'].forEach(function (bad) {
        const re = new RegExp('<option value="' + bad + '"');
        assert.ok(!re.test(src), 'непроверенный backend раскладки вернулся в форму: ' + bad);
    });
});

check('R118: split mode ограничен проверенными значениями (LAYER)', function () {
    const list = M.GGUF_VERIFIED_SPLIT_MODES || [];
    const values = list.map(function (o) { return Number(o.value); });
    assert.deepStrictEqual(values, [-1, 1],
        'в списке split mode остались непроверенные режимы: ' + values.join(', '));
    assert.ok(values.indexOf(1) !== -1, 'LAYER (1) — режим раскладки слоёв — обязан быть');
    const src = fs.readFileSync(path.join(__dirname, 'gguf-renderer-detail-render.js'), 'utf8');
    assert.ok(!/2=ROW/.test(src), 'подсказка всё ещё рекламирует ROW');
});

console.log('\nOK: ' + checks + ' checks passed');
