// bulk-models-refresh-r686.test.js — R686 (2026-11-22): после массовой операции
// список моделей ОБНОВЛЯЕТСЯ, а результат виден оператору.
//
// ЖИВОЙ ДЕФЕКТ. В modules/bulk-models.js было два мёртвых guard'а:
//   * после Load/Unload/Delete Selected вызывался `window.ui.refresh()`, но
//     такого метода в `ui` НЕТ (ui — возвращаемый объект IIFE app.js), поэтому
//     карточки моделей оставались со старым состоянием до следующего тика;
//   * уведомление искали в `window.ui.showNotification` — его тоже нет, и
//     результат массовой операции уходил только в console.
//
// ЧТО СТАЛО. Обновление идёт существующим путём — DataRefresh.refresh('models')
// (провайдер 'models' в app-data.js:628: перечитывает cluster state, рисует
// карточки и перерисовывает toolbar), а уведомление — каноническим
// window.showToast (app-core.js:280). Резервный путь обновления —
// App.context.refreshPage('models').
//
// Запуск: node webui/tests/bulk-models-refresh-r686.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const MODULE = path.join(__dirname, '..', 'js/modules/bulk-models.js');
const src = fs.readFileSync(MODULE, 'utf8');

let passed = 0;
const failures = [];
function check(name, fn) {
    return Promise.resolve().then(fn).then(function () {
        passed++;
        console.log('  \u2713 ' + name);
    }).catch(function (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' — ' + (e && e.message));
    });
}

// --- минимальное окружение (модуль — синглтон, DOM нужен только функциям) ---
global.window = global;
global.window.addEventListener = function () {};
global.document = {
    querySelectorAll: function () { return []; },
    getElementById: function () { return null; }
};

const calls = { refresh: [], toasts: [], ops: [], bulk: [] };

global.window.DataRefresh = {
    refresh: function (page) { calls.refresh.push(page); return Promise.resolve(true); }
};
global.window.showToast = function (msg, type) { calls.toasts.push({ msg: msg, type: type }); };
global.window.Api = {
    clusterModels: {
        bulk: function (body) { calls.bulk.push(body); return Promise.resolve({ succeeded: 1, failed: 0 }); }
    },
    backendModelOperation: function (backendId, op, model) {
        calls.ops.push({ backendId: backendId, op: op, model: model });
        return Promise.resolve({ success: true });
    },
    handleError: function () {}
};

new Function(src)();
assert.ok(global.window.bulkModels, 'bulk-models.js не экспортировал window.bulkModels');

function resetCalls() {
    calls.refresh.length = 0;
    calls.toasts.length = 0;
    calls.ops.length = 0;
    calls.bulk.length = 0;
}

(async function main() {
    await check('Load Selected: вызывает DataRefresh.refresh(models) и тост успеха', async function () {
        resetCalls();
        window.bulkModels._selected.clear();
        window.bulkModels._selected.add('b1::m1');
        window.bulkModels._selected.add('b2::m2');
        await window.bulkModels.execute('load');
        assert.deepStrictEqual(calls.refresh, ['models'],
            'после bulk-операции список моделей не обновляется (ожидался реальный путь DataRefresh.refresh("models"))');
        assert.strictEqual(calls.toasts.length, 1, 'результат операции не показан оператору');
        assert.deepStrictEqual(calls.toasts[0], { msg: 'succeeded=1 failed=0', type: 'success' },
            'тост ушёл не через window.showToast/с неверным типом');
        assert.strictEqual(calls.bulk.length, 1, 'bulk-endpoint не вызван');
        assert.strictEqual(window.bulkModels._selected.size, 0, 'выбор не очищен после операции');
    });

    await check('Unload Selected: тоже обновляет список моделей', async function () {
        resetCalls();
        window.bulkModels._selected.clear();
        window.bulkModels._selected.add('b1::m1');
        await window.bulkModels.execute('unload');
        assert.deepStrictEqual(calls.refresh, ['models'], 'unload не обновляет список моделей');
        assert.strictEqual(calls.bulk[0].operation, 'unload', 'неверная операция в теле запроса');
    });

    await check('Delete Selected: per-backend удаление + обновление + тост', async function () {
        resetCalls();
        window.bulkModels._selected.clear();
        window.bulkModels._selected.add('b1::m1');
        window.bulkModels._selected.add('b1::m2');
        await window.bulkModels.execute('delete');
        assert.strictEqual(calls.ops.length, 2, 'удаление должно идти по каждому бэкенду/модели');
        assert.deepStrictEqual(calls.refresh, ['models'], 'delete не обновляет список моделей');
        assert.strictEqual(calls.toasts.length, 1, 'результат удаления не показан оператору');
        assert.deepStrictEqual(calls.toasts[0], { msg: 'Delete: succeeded=2 failed=0', type: 'success' },
            'тост удаления ушёл не через window.showToast');
    });

    await check('ошибка bulk-операции: тост типа error, обновление не требуется', async function () {
        resetCalls();
        const goodBulk = window.Api.clusterModels.bulk;
        window.Api.clusterModels.bulk = function () { return Promise.reject(new Error('boom')); };
        try {
            window.bulkModels._selected.clear();
            window.bulkModels._selected.add('b1::m1');
            await window.bulkModels.execute('load');
        } finally {
            window.Api.clusterModels.bulk = goodBulk;
        }
        assert.strictEqual(calls.toasts.length, 1, 'ошибка не показана оператору');
        assert.strictEqual(calls.toasts[0].type, 'error', 'ошибка показана как успех');
        assert.ok(calls.toasts[0].msg.indexOf('boom') !== -1, 'в тосте нет причины ошибки');
    });

    await check('в модуле не осталось мёртвых обращений к window.ui', function () {
        assert.strictEqual(src.indexOf('window.ui'), -1,
            'в bulk-models.js снова читается window.ui (таких методов в ui нет — ветка будет мёртвой)');
        assert.strictEqual(src.indexOf('showNotification'), -1,
            'в bulk-models.js снова ищется showNotification (его нет; канонический тост — window.showToast)');
    });

    console.log('\n' + passed + ' проверок зелёные, ' + failures.length + ' красных');
    if (failures.length > 0) {
        failures.forEach(function (f) { console.error('  FAIL: ' + f); });
        process.exit(1);
    }
})().catch(function (e) {
    console.error('FAIL: ' + (e && e.message));
    process.exit(1);
});
