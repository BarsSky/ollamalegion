// monitor-vram-window-r594.test.js — R594 (2026-11-19).
//
// Запуск: node webui/js/modules/monitor-vram-window-r594.test.js
//
// ЖИВОЙ ФАКТ, ради которого это есть. 27B/A10, diag-архив 2026-11-19: мост
// сообщил `max_vram_n_ctx=251254` (вся свободная VRAM / KV на токен — без резерва),
// балансер принял решение по числу С резервом, окно подняли до 201216, после
// загрузки свободно 414 MB из 22 502, `CUDA0 compute buffer = 316.75 MiB`, декод
// промпта 33 812 токенов упал с `llama_decode ... code 1`.
//
// Что фиксируем:
//   1. в карточке бэкенда видны ОБА потолка и размер резерва;
//   2. если загруженное окно выше потолка с резервом — есть предупреждение
//      (именно это предшествует падению декода);
//   3. без данных строка не рисуется вовсе — нули не выдаются за факт;
//   4. числа экранируются/форматируются, мусор во входе не ломает отрисовку.

'use strict';

const assert = require('assert');

global.window = global;
global.MonitorApp = {
    esc: function (s) {
        return String(s === null || s === undefined ? '' : s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    },
    fmtDur: function () { return '-'; }
};
global.I18N = { t: function (k, d) { return d || k; } };
global.document = {
    querySelector: function () { return null; },
    getElementById: function () { return null; },
    querySelectorAll: function () { return []; },
    createElement: function () { return { style: {}, dataset: {}, addEventListener: function () {} }; },
    addEventListener: function () {}
};

const fs = require('fs');
const path = require('path');
const NodeModule = require('module');
const SRC = path.join(__dirname, 'monitor-backends.js');
const m = new NodeModule(SRC, module);
m._compile(fs.readFileSync(SRC, 'utf8') + '\nmodule.exports = MonitorBackends;', SRC);
const MB = m.exports;

let checks = 0;
function check(name, fn) { fn(); checks++; console.log('  ✓ ' + name); }

// Живой фикстур: числа из diag-архива 27B/A10.
const LIVE = {
    id: 'cppworker-gpu-bundled-agent',
    type: 'llama_cpp',
    vramWindow: { maxVramNCtx: 201216, maxVramNCtxUnreserved: 251254, reserveMB: 2048 },
    loadedModels: [{ name: 'Qwen3.8-27B-UD-Q4_K_M', contextLength: 201216 }]
};

check('R594: оба потолка и резерв видны в карточке', function () {
    const html = MB.vramWindowNote(LIVE);
    assert.ok(html.length > 0, 'строка не нарисована');
    assert.ok(/201\D?216/.test(html), 'нет применяемого потолка (с резервом): ' + html);
    assert.ok(/251\D?254/.test(html), 'нет безрезервного потолка (число моста): ' + html);
    assert.ok(/2\D?048/.test(html), 'нет размера резерва: ' + html);
    assert.ok(/vram-window-note/.test(html), 'нет класса строки');
});

check('R594: окно выше потолка с резервом → предупреждение', function () {
    // Окно равно применяемому потолку в живом фикстуре; для проверки ветки берём
    // окно больше потолка — так выглядит состояние после роста окна по числу моста.
    const over = Object.assign({}, LIVE, {
        vramWindow: { maxVramNCtx: 113000, maxVramNCtxUnreserved: 251254, reserveMB: 2048 },
        loadedModels: [{ name: 'Qwen3.8-27B-UD-Q4_K_M', contextLength: 201216 }]
    });
    const html = MB.vramWindowNote(over);
    assert.ok(/выше потолка/.test(html), 'нет предупреждения: ' + html);
    assert.ok(/vram-window-note-over/.test(html), 'нет класса предупреждения');
});

check('R594: окно в пределах потолка — предупреждения нет', function () {
    const html = MB.vramWindowNote(LIVE);
    assert.ok(!/выше потолка/.test(html), 'ложное предупреждение: ' + html);
    assert.ok(!/vram-window-note-over/.test(html), 'лишний класс предупреждения');
});

check('R594: без данных строка не рисуется (нули не выдаются за факт)', function () {
    assert.strictEqual(MB.vramWindowNote({ id: 'x', type: 'llama_cpp' }), '', 'нарисована строка без данных');
    assert.strictEqual(MB.vramWindowNote({ vramWindow: { maxVramNCtx: 0, maxVramNCtxUnreserved: 0, reserveMB: 0 } }), '',
        'нули выданы за факт');
    assert.strictEqual(MB.vramWindowNote(null), '', 'падение/строка на null');
});

check('R594: мусор в числах не ломает отрисовку и экранируется', function () {
    const html = MB.vramWindowNote({
        vramWindow: { maxVramNCtx: 'abc', maxVramNCtxUnreserved: 251254, reserveMB: 0 },
        loadedModels: [{ contextLength: '<script>' }]
    });
    assert.ok(html.indexOf('251') !== -1, 'потеряно единственное валидное число: ' + html);
    assert.ok(html.indexOf('<script>') === -1, 'не экранировано: ' + html);
});

console.log('\n' + checks + ' проверок, все зелёные');
