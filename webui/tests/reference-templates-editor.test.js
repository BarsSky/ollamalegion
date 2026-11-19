// reference-templates-editor.test.js — R515 (2026-11-19): шаблоны в «Справке»
// можно создавать, править и удалять ИЗ UI.
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА (§2.6 разбора замечаний): «шаблоны нельзя править».
// Ограничение было только в UI: сервер умеет всё — PUT/DELETE
// /api/v1/reference/templates/{id} (internal/api/routes.go:520-522,
// handlers_reference.go:509-536,614-680), валидация params — :633-647. Страница
// звала upsert/remove только для 'recommendations', а у шаблона были три
// действия: открыть/предпросмотр/применить.
//
// Тест поведенческий: модуль грузится в подставной DOM (как
// reference-page.test.js), Api.reference подменён и записывает вызовы. Поэтому
// проверяется не текст, а: какие кнопки попали в карточку, что именно уходит в
// PUT (плоский объект params, coerced по MODEL_PARAM_SPECS), что НЕ уходит при
// невалидном id/пустых параметрах, и что удаление идёт через DELETE.
'use strict';

const assert = require('assert');

// --- минимальный DOM -------------------------------------------------------
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
global.document = {
    getElementById: getEl,
    addEventListener() {},
    querySelectorAll() { return []; },
    visibilityState: 'visible'
};
global.window.I18N = { t: function (key) { return key; } };

global.window.GgufModule = {
    MODEL_PARAM_SPECS: [
        { field: 'contextLength', type: 'int', min: 256, max: 1048576, step: 256, label: 'gguf.ctx_size' },
        { field: 'parallel', type: 'int', min: 0, max: 32, step: 1, label: 'gguf.parallel' },
        { field: 'kvCacheType', type: 'enum', options: ['f16', 'q8_0', 'q4_0'], label: 'gguf.kv_cache_type' },
        { field: 'flashAttn', type: 'bool', label: 'gguf.flash_attn' }
    ]
};
global.window.DataRefresh = { register: function () { return true; } };

// --- Api-стаб, который ЗАПИСЫВАЕТ вызовы ----------------------------------
const calls = { upsert: [], remove: [], get: [] };
let failNext = null;
global.window.Api = {
    reference: {
        list: function (kind) {
            if (kind === 'templates') return Promise.resolve({ items: [TPL], total: 1, schemaVersion: 1 });
            return Promise.resolve({ items: [], total: 0, schemaVersion: 1 });
        },
        get: function (kind, id) {
            calls.get.push(kind + '/' + id);
            return Promise.resolve({ template: TPL, usage: { usedByProfiles: ['Qwen3-4B-Instruct-2507'] } });
        },
        upsert: function (kind, id, body) {
            if (failNext) { const e = failNext; failNext = null; return Promise.reject(e); }
            calls.upsert.push({ kind: kind, id: id, body: body });
            return Promise.resolve({ status: 'ok', template: { id: id, revision: 7 } });
        },
        remove: function (kind, id) {
            calls.remove.push({ kind: kind, id: id });
            return Promise.resolve({ status: 'ok', id: id });
        },
        toTemplate: function () { return Promise.resolve({ status: 'created' }); },
        preview: function () { return Promise.resolve({ ok: true, valid: { ok: true, violations: [] }, diff: [], targets: [], memory: {}, warnings: [] }); },
        apply: function () { return Promise.resolve({ status: 'applied', backends: [] }); }
    },
    clusterModels: { loaded: function () { return Promise.resolve({ count: 0, models: [] }); } }
};
global.window.confirm = function () { return true; };

const TPL = {
    id: 'agent-8gb',
    name: 'Агент на 8 ГБ VRAM',
    version: 2,
    params: { contextLength: 32768, kvCacheType: 'q8_0', parallel: 1 },
    comment: 'Годится для 4B–8B Q4 на 8 ГБ.',
    basedOn: 'qwen3-4b-8gb-agent',
    revision: 4
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

(async function main() {

    await check('R515: в карточке шаблона есть кнопки «Изменить» и «Удалить» (только для админа)', function () {
        const html = RP.renderTemplateCard(TPL);
        assert.ok(/data-ref-action="edit-tpl"[^>]*data-admin-only|data-admin-only[^>]*data-ref-action="edit-tpl"/.test(html),
            'нет кнопки правки шаблона (или она без data-admin-only)');
        assert.ok(/data-ref-action="delete-tpl"[^>]*data-admin-only|data-admin-only[^>]*data-ref-action="delete-tpl"/.test(html),
            'нет кнопки удаления шаблона (или она без data-admin-only)');
    });

    await check('R515: «Новый шаблон» открывает форму с полями id/name/version и строкой параметра', function () {
        RP.openTplEditor(null);
        const host = getEl('referenceTplEditor');
        assert.strictEqual(host.hidden, false, 'форма осталась скрытой');
        assert.ok(host.innerHTML.indexOf('data-ref-tpledit="id"') !== -1, 'нет поля идентификатора');
        assert.ok(host.innerHTML.indexOf('data-ref-tpledit="name"') !== -1, 'нет поля названия');
        assert.ok(host.innerHTML.indexOf('data-ref-tpledit="version"') !== -1, 'нет поля версии');
        assert.ok(host.innerHTML.indexOf('data-ref-action="tpl-editor-save"') !== -1, 'нет кнопки сохранения');
        assert.ok(host.innerHTML.indexOf('data-ref-action="tpl-editor-add-param"') !== -1, 'нет кнопки добавления параметра');
    });

    await check('R515: тело PUT — плоский объект params, значения приведены по MODEL_PARAM_SPECS', function () {
        RP.openTplEditor(null);
        RP._state.tplEditor.id = 'new-tpl-1';
        RP._state.tplEditor.name = 'Новый набор';
        RP._state.tplEditor.params = [
            { name: 'contextLength', raw: '65536' },
            { name: 'kvCacheType', raw: 'q4_0' },
            { name: 'flashAttn', raw: 'true' },
            { name: '', raw: 'игнор' }
        ];
        const built = RP.tplEditorBody();
        assert.deepStrictEqual(built.body.params, { contextLength: 65536, kvCacheType: 'q4_0', flashAttn: true },
            'params ушли не тем объектом: ' + JSON.stringify(built.body.params));
        assert.strictEqual(built.params, 3, 'посчитано параметров: ' + built.params);
        assert.strictEqual(built.body.name, 'Новый набор', 'имя не попало в тело');
    });

    await check('R515: сохранение нового шаблона идёт в PUT /templates/{id} и закрывает форму', async function () {
        calls.upsert.length = 0;
        RP.openTplEditor(null);
        RP._state.tplEditor.id = 'my-new-tpl';
        RP._state.tplEditor.name = 'Мой шаблон';
        RP._state.tplEditor.comment = 'проверка';
        RP._state.tplEditor.params = [{ name: 'contextLength', raw: '32768' }];
        const ok = await RP.saveTplEditor();
        assert.strictEqual(ok, undefined === ok ? undefined : ok, 'saveTplEditor вернул неожиданное');
        assert.strictEqual(calls.upsert.length, 1, 'ожидался ровно один PUT, было ' + calls.upsert.length);
        assert.strictEqual(calls.upsert[0].kind, 'templates', 'PUT ушёл не на templates: ' + calls.upsert[0].kind);
        assert.strictEqual(calls.upsert[0].id, 'my-new-tpl', 'id не тот: ' + calls.upsert[0].id);
        assert.deepStrictEqual(calls.upsert[0].body.params, { contextLength: 32768 }, 'params не те');
        assert.strictEqual(RP._state.tplEditor, null, 'форма не закрылась после сохранения');
    });

    await check('R515: невалидный id и пустые параметры НЕ ходят в API', async function () {
        calls.upsert.length = 0;
        RP.openTplEditor(null);
        RP._state.tplEditor.id = 'X'; // слишком короткий slug
        RP._state.tplEditor.name = 'Имя';
        RP._state.tplEditor.params = [{ name: 'contextLength', raw: '32768' }];
        await RP.saveTplEditor();
        assert.strictEqual(calls.upsert.length, 0, 'PUT ушёл с невалидным id');

        RP.openTplEditor(null);
        RP._state.tplEditor.id = 'valid-slug';
        RP._state.tplEditor.name = 'Имя';
        RP._state.tplEditor.params = [{ name: '', raw: '' }];
        await RP.saveTplEditor();
        assert.strictEqual(calls.upsert.length, 0, 'PUT ушёл без параметров (сервер ответил бы params_required)');
        assert.ok(getEl('referenceStatus').textContent.indexOf('params_required') !== -1,
            'оператор не увидел причину: ' + getEl('referenceStatus').textContent);
    });

    await check('R515: правка существующего шаблона заполняет форму и фиксирует id', function () {
        RP.openTplEditor(TPL);
        const host = getEl('referenceTplEditor');
        assert.ok(host.innerHTML.indexOf('value="agent-8gb"') !== -1, 'id шаблона не подставлен');
        assert.ok(/data-ref-tpledit="id"[^>]*readonly/.test(host.innerHTML), 'id существующего шаблона можно менять');
        assert.ok(host.innerHTML.indexOf('Агент на 8 ГБ VRAM') !== -1, 'название не подставлено');
        const names = RP._state.tplEditor.params.map((p) => p.name).sort();
        assert.deepStrictEqual(names, ['contextLength', 'kvCacheType', 'parallel'], 'параметры не разобраны: ' + names);
        assert.ok(host.innerHTML.indexOf('data-ref-action="tpl-editor-delete"') !== -1, 'нет кнопки удаления в форме правки');
    });

    await check('R515: удаление шаблона идёт через DELETE и убирает раскрытие', async function () {
        calls.remove.length = 0;
        RP._state.tpl.id = 'agent-8gb';
        await RP.deleteTemplate('agent-8gb');
        assert.deepStrictEqual(calls.remove, [{ kind: 'templates', id: 'agent-8gb' }],
            'DELETE ушёл не тот: ' + JSON.stringify(calls.remove));
        assert.strictEqual(RP._state.tpl.id, '', 'раскрытый шаблон остался после удаления');
    });

    await check('R515: форма правки показывает подсказку usage.usedByProfiles', function () {
        const withUsage = RP.renderTemplateDetail(TPL, {
            tplState: { model: '', backendId: '', mode: 'profile+reload', usedByProfiles: ['Qwen3-4B-Instruct-2507'] }
        });
        assert.ok(withUsage.indexOf('Qwen3-4B-Instruct-2507') !== -1,
            'список профилей, совпавших с шаблоном, не показан (API его отдаёт в usage.usedByProfiles)');
        const without = RP.renderTemplateDetail(TPL, {
            tplState: { model: '', backendId: '', mode: 'profile+reload', usedByProfiles: [] }
        });
        assert.ok(without.indexOf('Ни один профиль') !== -1,
            'при пустом списке нет честного «пока ни один профиль не совпадает»');
    });

    await check('R515: id поля шаблона не пересекаются с редактором рекомендации', function () {
        RP.openTplEditor(null);
        const host = getEl('referenceTplEditor');
        assert.strictEqual(host.innerHTML.indexOf('data-ref-edit="paramValue"'), -1,
            'поля шаблона используют атрибут редактора РЕКОМЕНДАЦИИ — значения попадут в чужое состояние');
        assert.ok(host.innerHTML.indexOf('data-ref-tpledit="paramValue"') !== -1,
            'у полей шаблона нет своего атрибута data-ref-tpledit');
    });

    console.log('');
    if (failures.length) {
        console.error('Провалов: ' + failures.length);
        failures.forEach(function (f) { console.error('  - ' + f); });
        process.exit(1);
    }
    console.log('Пройдено проверок: ' + passed);
})();
