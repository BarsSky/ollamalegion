// reference-targets-catalog.test.js — R516 (2026-11-19): селектор «где поднять»
// больше не ждёт, пока модель где-то загрузится.
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА (§2.7 разбора): «где поднять» ждёт бэкенд с загруженной
// моделью, хотя сервер разрешает применять шаблон на узел, где файл модели лежит
// на диске. Причина: `loadedTargetsFor` брал цели ТОЛЬКО из
// `GET /api/v1/cluster/models/loaded`, а серверу достаточно существования бэкенда
// и файла (`handlers_reference_apply.go:294-324,368-391`). Плюс `applyGate`
// блокировал режим «только профиль» при пустом списке, хотя серверу для него
// бэкенд не нужен вовсе.
//
// Узел НЕ угадываем: каталог обязан назвать свой `backendId` (R516 в API). Без
// него дополнительных целей не появляется — лучше меньше целей, чем узел, где
// файла нет.
'use strict';

const assert = require('assert');

function makeElement(id) {
    const classes = new Set();
    return {
        id: id, hidden: false, innerHTML: '', textContent: '', className: '', style: {},
        classList: {
            add(c) { classes.add(c); }, remove(c) { classes.delete(c); },
            contains(c) { return classes.has(c); }, toggle(c, on) { if (on) classes.add(c); else classes.delete(c); }
        },
        addEventListener() {}, querySelector() { return null; }, querySelectorAll() { return []; },
        getAttribute() { return null; }, setAttribute() {}, appendChild() {},
        insertAdjacentHTML() {}, scrollIntoView() {}
    };
}
const elements = {};
function getEl(id) {
    if (!Object.prototype.hasOwnProperty.call(elements, id)) elements[id] = makeElement(id);
    return elements[id];
}
global.window = global;
global.document = { getElementById: getEl, addEventListener() {}, querySelectorAll() { return []; }, visibilityState: 'visible' };
global.window.I18N = { t: function (key) { return key; } };
global.window.GgufModule = { MODEL_PARAM_SPECS: [{ field: 'contextLength', type: 'int', min: 256, max: 1048576, step: 256, label: 'gguf.ctx_size' }] };
global.window.DataRefresh = { register: function () { return true; } };
global.window.confirm = function () { return true; };

const MODEL = 'Qwen3-14B-Instruct-2507';
const LOADED_BACKEND = 'cppworker-loaded';
const DISK_BACKEND = 'cppworker-on-disk';
const catalogCalls = { count: 0 };
let catalogBackendId = DISK_BACKEND;

global.window.Api = {
    reference: {
        list: function () { return Promise.resolve({ items: [], total: 0, schemaVersion: 1 }); },
        get: function () { return Promise.resolve({}); },
        upsert: function () { return Promise.resolve({ status: 'ok' }); },
        remove: function () { return Promise.resolve({ status: 'ok' }); },
        toTemplate: function () { return Promise.resolve({ status: 'created' }); },
        preview: function () { return Promise.resolve({ ok: true, valid: { ok: true, violations: [] }, diff: [], targets: [], memory: {}, warnings: [] }); },
        apply: function () { return Promise.resolve({ status: 'applied', backends: [] }); }
    },
    clusterModels: {
        loaded: function () {
            return Promise.resolve({
                count: 1,
                models: [{ name: MODEL, backendId: LOADED_BACKEND, backendType: 'llama_cpp', contextLength: 32768 }]
            });
        }
    },
    cppworkerLoadDefaults: {
        catalog: function () {
            catalogCalls.count++;
            return Promise.resolve({
                backendId: catalogBackendId,
                files: [{ name: MODEL + '.gguf', sizeBytes: 8 * 1024 * 1024 * 1024 }]
            });
        }
    }
};

// R642: состояние и помощники вынесены и грузятся первыми (как в index.html).
require('../js/modules/reference-page-helpers.js');
require('../js/modules/reference-page.js');
const RP = global.window.ReferencePage;
assert.ok(RP, 'window.ReferencePage не экспортирован');

let passed = 0;
const failures = [];
async function check(name, fn) {
    try { await fn(); passed++; console.log('  \u2713 ' + name); }
    catch (e) { failures.push(name + ': ' + (e && e.message)); console.error('  \u2717 ' + name + ' — ' + (e && e.message)); }
}

const LOADED = [{ name: MODEL, backendId: LOADED_BACKEND, backendType: 'llama_cpp', contextLength: 32768 }];
const CATALOG = { backendId: DISK_BACKEND, files: [{ name: MODEL + '.gguf', sizeBytes: 123 }] };

(async function main() {

    await check('R516: цель «файл на диске» добавляется к загруженным (union)', function () {
        const targets = RP.loadedTargetsFor(LOADED, MODEL, CATALOG);
        assert.strictEqual(targets.length, 2, 'целей: ' + targets.length + ' (' + targets.map((t) => t.backendId).join(', ') + ')');
        const disk = targets.filter((t) => t.backendId === DISK_BACKEND)[0];
        assert.ok(disk, 'узла с файлом на диске нет среди целей');
        assert.strictEqual(disk.loaded, false, 'цель с диска помечена загруженной');
        assert.strictEqual(disk.fromCatalog, true, 'цель не помечена как «из каталога»');
    });

    await check('R516: каталог без имени узла НЕ добавляет целей (узел не выдумываем)', function () {
        const targets = RP.loadedTargetsFor(LOADED, MODEL, { backendId: '', files: CATALOG.files });
        assert.strictEqual(targets.length, 1, 'без backendId появились лишние цели: ' + targets.map((t) => t.backendId).join(', '));
    });

    await check('R516: чужой файл не превращается в цель для этой модели', function () {
        const targets = RP.loadedTargetsFor(LOADED, MODEL, { backendId: DISK_BACKEND, files: [{ name: 'other-model.gguf' }] });
        assert.strictEqual(targets.length, 1, 'добавился узел для модели, которой там нет');
    });

    await check('R516: модель с диска можно ВЫБРАТЬ (её нет в /cluster/models/loaded)', function () {
        RP._state.loadedModels = [MODEL];
        RP._state.catalog = CATALOG;
        const choices = RP.modelChoices('');
        assert.ok(choices.indexOf(MODEL) !== -1, 'загруженная модель потерялась: ' + choices.join(', '));
        // вторая модель: только на диске
        RP._state.loadedModels = ['SomeOtherLoaded'];
        const choices2 = RP.modelChoices('');
        assert.ok(choices2.indexOf(MODEL) !== -1,
            'модели с диска нет в выборе — «сначала загрузите модель» осталось: ' + choices2.join(', '));
    });

    await check('R516: gate разрешает узел «файл на диске» и предупреждает о загрузке', function () {
        const gate = RP.applyGate({ model: MODEL, backendId: DISK_BACKEND, mode: 'profile+reload', targets: RP.loadedTargetsFor(LOADED, MODEL, CATALOG) });
        assert.strictEqual(gate.disabled, false, 'кнопка заблокирована, хотя сервер разрешает: ' + gate.reason);
        assert.strictEqual(gate.warn, 'will_load', 'нет предупреждения «будет загрузка»: ' + JSON.stringify(gate));
    });

    await check('R516: gate по-прежнему отказывает, если выбранного узла нет среди целей', function () {
        const gate = RP.applyGate({ model: MODEL, backendId: 'cppworker-unknown', mode: 'profile+reload', targets: RP.loadedTargetsFor(LOADED, MODEL, CATALOG) });
        assert.strictEqual(gate.disabled, true, 'разрешён узел, которого нет в целях');
        assert.strictEqual(gate.reason, 'backend_not_loaded', 'причина: ' + gate.reason);
    });

    await check('R516: profile-only не требует бэкенда даже при пустом списке целей', function () {
        const gate = RP.applyGate({ model: MODEL, backendId: '', mode: 'profile-only', targets: [] });
        assert.strictEqual(gate.disabled, false, 'profile-only снова заблокирован «нет целей»: ' + gate.reason);
    });

    await check('R516: без целей вообще profile+reload остаётся заблокированным (не «разрешить всё»)', function () {
        const gate = RP.applyGate({ model: MODEL, backendId: '', mode: 'profile+reload', targets: [] });
        assert.strictEqual(gate.disabled, true, 'применение без единой цели разрешено');
        assert.strictEqual(gate.reason, 'no_loaded_target', 'причина: ' + gate.reason);
    });

    await check('R516: предупреждение и подпись узла видны в детали шаблона', function () {
        const targets = RP.loadedTargetsFor(LOADED, MODEL, CATALOG);
        const html = RP.renderTemplateDetail(
            { id: 't1', name: 'T', version: 1, params: { contextLength: 32768 }, revision: 1 },
            { tplState: { model: MODEL, backendId: DISK_BACKEND, mode: 'profile+reload' }, models: [MODEL], targets: targets }
        );
        assert.ok(html.indexOf('is-warning') !== -1, 'предупреждение о загрузке не показано (нет класса is-warning)');
        assert.ok(/начнётся с её загрузки/.test(html), 'нет человеческого текста «будет загрузка»');
        assert.ok(html.indexOf('файл на диске') !== -1, 'узел не подписан как «файл на диске, не загружена»');
        const applyBtn = (html.match(/<button[^>]*data-ref-action="apply"[^>]*>/) || [])[0] || '';
        assert.ok(applyBtn.indexOf('disabled') === -1, 'кнопка «Применить» осталась неактивной: ' + applyBtn);
    });

    await check('R516: load() запрашивает каталог и запоминает имя узла', async function () {
        catalogBackendId = DISK_BACKEND;
        catalogCalls.count = 0;
        await RP.load();
        assert.ok(catalogCalls.count >= 1, 'каталог не запрашивался вовсе');
        assert.strictEqual(RP._state.catalog.backendId, DISK_BACKEND,
            'backendId каталога не сохранён: ' + JSON.stringify(RP._state.catalog.backendId));
        assert.ok(RP._state.catalog.files.length >= 1, 'файлы каталога не сохранены');
    });

    console.log('');
    if (failures.length) {
        console.error('Провалов: ' + failures.length);
        failures.forEach(function (f) { console.error('  - ' + f); });
        process.exit(1);
    }
    console.log('Пройдено проверок: ' + passed);
})();
