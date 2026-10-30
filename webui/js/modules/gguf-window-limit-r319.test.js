// gguf-window-limit-r319.test.js — R319 (2026-10-30).
//
// Запуск: node webui/js/modules/gguf-window-limit-r319.test.js
//
// ЖИВАЯ ЖАЛОБА (повторялась несколько раз): «модель обучена на 131072, VRAM даёт
// 131072 — почему фактическое окно 32768?». В интерфейсе были два первых числа
// (feasible/GGUF, R83) и ни одного упоминания третьего — потолка авто-роста
// LB_NCTX_RELOAD_MAX_N_CTX. Балансер уже отдавал причину в GET model-profiles
// (`effectiveContext[<модель>].limitReason` и `nctxCeiling`), но фронт забирал из
// ответа только `models`.
//
// Что проверяем:
//   1. запись профиля с limitedBy="env" → тон warn, число и env в тексте;
//   2. запись профиля без limitedBy → «ограничивает профиль», тон normal;
//   3. записи нет, потолок задан → потолок показан (модель без профиля не остаётся
//      без объяснения — ради этого добавлялось nctxCeiling);
//   4. ни профиля, ни потолка → честный текст про VRAM/GGUF, а не пустота;
//   5. modelEffectiveEntry находит запись по имени файла и по пути;
//   6. paintWindowLimit создаёт ровно один блок и обновляет его повторно.

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

global.I18N = { t: function (key, fallback) { return fallback === undefined ? key : fallback; } };
global.window.I18N = global.I18N;

// Минимальный DOM: paintWindowLimit ищет .gguf-mp-actions/.gguf-mp-limit внутри
// формы и вставляет блок перед действиями.
function fakeEl(attrs) {
    const el = {
        _attrs: attrs || {},
        children: [],
        className: '',
        textContent: '',
        _found: {},
        getAttribute: function (n) { return Object.prototype.hasOwnProperty.call(this._attrs, n) ? this._attrs[n] : null; },
        setAttribute: function (n, v) { this._attrs[n] = v; },
        querySelector: function (sel) {
            if (Object.prototype.hasOwnProperty.call(this._found, sel)) return this._found[sel];
            if (sel === '.gguf-mp-limit') return this._limit || null;
            if (sel === '.gguf-mp-actions') return this._actions || null;
            return null;
        },
        appendChild: function (c) { this.children.push(c); return c; },
        insertBefore: function (c) { this.children.push(c); return c; }
    };
    return el;
}

global.document = {
    querySelector: function () { return null; },
    querySelectorAll: function () { return []; },
    getElementById: function () { return null; },
    createElement: function () { return fakeEl({}); },
    addEventListener: function () {}
};

const path = require('path');
require(path.join(__dirname, 'gguf-renderer-detail-render.js'));
const M = global.window.GgufModule;

let failures = 0;
function test(name, fn) {
    try {
        fn();
        console.log('✔ ' + name);
    } catch (e) {
        failures++;
        console.log('✖ ' + name + '\n    ' + e.message);
    }
}

// ── 1. Профиль + env-потолок ────────────────────────────────────────────────
test('R319: запись профиля с limitedBy=env даёт warn и оба числа', function () {
    const entry = {
        effectiveContextLength: 32768,
        limitedBy: 'env',
        limitReason: 'окно модели задано 16384, но LB_NCTX_RELOAD_MAX_N_CTX=32768 разрешает авто-рост только до него',
        envCeiling: 32768
    };
    const note = M.windowLimitNote(entry, { envCeiling: 32768, limitedBy: 'env', reason: 'x' });
    assert.strictEqual(note.tone, 'warn', 'тон должен предупреждать: окно режет env');
    assert.ok(/Предел окна: 32768/.test(note.text), 'нет числа предела: ' + note.text);
    assert.ok(/LB_NCTX_RELOAD_MAX_N_CTX=32768/.test(note.text), 'нет имени переменной с числом: ' + note.text);
    assert.ok(/разрешает авто-рост только до него/.test(note.text), 'причина балансера потеряна: ' + note.text);
});

// ── 2. Профиль без env-ограничения ─────────────────────────────────────────
test('R319: профиль без env-ограничения назван профилем, без warn', function () {
    const note = M.windowLimitNote({
        effectiveContextLength: 65536,
        limitReason: 'окно задано оператором в карточке модели; env-потолок 32768 не ограничивает'
    }, { envCeiling: 32768 });
    assert.strictEqual(note.tone, 'normal', 'профиль — это решение оператора, не предупреждение');
    assert.ok(/Предел окна: 65536/.test(note.text), note.text);
    assert.ok(/профиль/.test(note.text), 'не сказано, кто ограничивает: ' + note.text);
});

// ── 3. Профиля нет, потолок есть ───────────────────────────────────────────
test('R319: без профиля потолок всё равно виден (ради этого nctxCeiling)', function () {
    const note = M.windowLimitNote(null, {
        envCeiling: 32768,
        limitedBy: 'env',
        reason: 'потолок авто-роста окна LB_NCTX_RELOAD_MAX_N_CTX=32768'
    });
    assert.ok(/32768/.test(note.text), 'нет числа потолка: ' + note.text);
    assert.ok(/LB_NCTX_RELOAD_MAX_N_CTX/.test(note.text), 'нет источника потолка: ' + note.text);
});

// ── 4. Ничего не задано ────────────────────────────────────────────────────
test('R319: ни профиля, ни потолка — честный текст про VRAM/GGUF', function () {
    const note = M.windowLimitNote(null, { envCeiling: 0, reason: '' });
    assert.ok(/VRAM/.test(note.text) && /GGUF/.test(note.text), 'не сказано, что предел проверит загрузка: ' + note.text);
    assert.strictEqual(note.tone, 'normal', 'неизвестность — не предупреждение о неверной настройке');
});

// ── 5. Поиск записи по имени файла/пути ────────────────────────────────────
test('R319: modelEffectiveEntry находит запись по .gguf и по пути', function () {
    const map = {
        'gemma-4-E4B-it-Q4_K_M': { effectiveContextLength: 32768 },
        'qwen3-8b': { effectiveContextLength: 8192 }
    };
    const byBare = M.modelEffectiveEntry('gemma-4-E4B-it-Q4_K_M', map);
    assert.ok(byBare && byBare.effectiveContextLength === 32768, 'не нашёл по имени модели');
    const byFile = M.modelEffectiveEntry('gemma-4-E4B-it-Q4_K_M.gguf', map);
    assert.ok(byFile && byFile.effectiveContextLength === 32768, 'не нашёл по имени файла .gguf');
    const byPath = M.modelEffectiveEntry('/app/models/gemma-4-E4B-it-Q4_K_M.gguf', map);
    assert.ok(byPath && byPath.effectiveContextLength === 32768, 'не нашёл по пути');
    assert.strictEqual(M.modelEffectiveEntry('unknown-model', map), null, 'чужая модель не должна совпадать');
    assert.strictEqual(M.modelEffectiveEntry('x', null), null, 'пустая карта — null, а не падение');
});

// ── 6. Отрисовка: один блок, повторный вызов обновляет ─────────────────────
test('R319: paintWindowLimit создаёт один блок и обновляет его', function () {
    const actions = fakeEl({});
    const editor = fakeEl({ 'data-model': 'gemma-4-E4B-it-Q4_K_M' });
    editor._actions = actions;
    M.state = {
        modelEffectiveWindow: {
            'gemma-4-E4B-it-Q4_K_M': { effectiveContextLength: 32768, limitedBy: 'env', limitReason: 'причина' }
        },
        nctxCeiling: { envCeiling: 32768, limitedBy: 'env', reason: 'потолок' }
    };
    M.paintWindowLimit(editor);
    assert.strictEqual(editor.children.length, 1, 'блок должен появиться один раз');
    const box = editor.children[0];
    assert.strictEqual(box.className.indexOf('gguf-mp-limit-warn') >= 0, true, 'тон warn не передан в класс');
    assert.ok(/Предел окна: 32768/.test(box.textContent), 'текст блока: ' + box.textContent);
    assert.strictEqual(box.getAttribute('data-nctx-limit'), 'gemma-4-E4B-it-Q4_K_M', 'нет привязки к модели');

    // Повторная отрисовка (обновление профиля) не плодит блоки.
    editor._limit = box;
    M.paintWindowLimit(editor);
    assert.strictEqual(editor.children.length, 1, 'повторная отрисовка добавила второй блок');
    assert.ok(/Предел окна: 32768/.test(editor.children[0].textContent), 'блок не обновился');
});

// ── 7. Нет данных — блок не рисуется вовсе ─────────────────────────────────
test('R319: без данных блок не рисуется (не выдумываем причину)', function () {
    const editor = fakeEl({ 'data-model': 'x' });
    M.state = {};
    M.paintWindowLimit(editor);
    assert.strictEqual(editor.children.length, 0, 'пустое состояние не должно давать блок');
});

if (failures) {
    console.log('\n❌ FAILED: ' + failures);
    process.exit(1);
}
console.log('\n✅ PASS: gguf-window-limit-r319');
