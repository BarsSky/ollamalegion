// backend-type-filter.test.js — R-Image Phase 5 (2026-10-02): регресс на третий
// тип бэкенда image_cpp в фильтре типов.
//
// Запуск: node webui/js/modules/backend-type-filter.test.js
//
// Что проверяем (и почему это важно):
//   1. filterBackends(): image-бэкенд НЕ выбрасывается текстовым фильтром
//      (llama_cpp/ollama). Иначе зарегистрированный image-бэкенд исчезал бы со
//      страницы «Бэкенды» при backendEngine=llama_cpp (а он там нужен: его
//      видно, редактируют, удаляют).
//   2. filterBackends('image_cpp'): остаются только image-бэкенды.
//   3. getAvailableModes('image_cpp') = standard/replication/rpc_coordinator
//      (virtual_router/distributed_inference сознательно НЕ поддержаны - они
//      ломали резолв /api/*, см. план §7 Phase 1).
//   4. setCurrentType('image_cpp') — валидный UI-фильтр (localStorage + updateUI),
//      setCurrentType('мусор') игнорируется.
//   5. getCurrentType() читает 'image_cpp' из localStorage.
//   6. toggleBackendFormFields('image_cpp') показывает ТОЛЬКО .backend-field-image
//      (llama-поля скрыты, ollama-поля скрыты).
//
// Окружение: minimal DOM/localStorage-моки. IIFE модуля экспортирует
// window.BackendTypeFilter и не трогает DOM на этапе загрузки.

'use strict';

const assert = require('assert');

// --- mocks -----------------------------------------------------------------
global.window = global;

const store = {};
global.localStorage = {
    getItem: function (k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem: function (k, v) { store[k] = String(v); },
    removeItem: function (k) { delete store[k]; },
};

// Фейковые группы полей формы: только style.display, как в реальном DOM.
function fakeNodes(n) {
    const arr = [];
    for (let i = 0; i < n; i++) arr.push({ style: { display: '' } });
    return arr;
}
const FAKE_FIELDS = {
    '.backend-field-ollama': fakeNodes(1),
    '.backend-field-llama': fakeNodes(1),
    '.backend-field-image': fakeNodes(1),
    '.settings-section-ollama': [],
    '.settings-section-llama': [],
};
global.document = {
    querySelectorAll: function (sel) { return FAKE_FIELDS[sel] || []; },
    querySelector: function () { return null; },
    getElementById: function () { return null; },
};

require('./backend-type-filter.js');

const F = global.BackendTypeFilter;
assert.ok(F, 'window.BackendTypeFilter должен быть экспортирован');

let passed = 0;
function check(name, fn) {
    fn();
    passed++;
    console.log('  \u2713 ' + name);
}

const MIXED = [
    { id: 'llama-1', type: 'llama_cpp' },
    { id: 'ollama-1', type: 'ollama' },
    { id: 'img-1', type: 'image_cpp', imagePort: 18093 },
];

console.log('backend-type-filter (image_cpp)');

check('filterBackends(llama_cpp) сохраняет image-бэкенды', function () {
    const out = F.filterBackends(MIXED, 'llama_cpp').map(b => b.id);
    assert.deepStrictEqual(out, ['llama-1', 'img-1']);
});

check('filterBackends(ollama) сохраняет image-бэкенды', function () {
    const out = F.filterBackends(MIXED, 'ollama').map(b => b.id);
    assert.deepStrictEqual(out, ['ollama-1', 'img-1']);
});

check('filterBackends(image_cpp) оставляет только image-бэкенды', function () {
    const out = F.filterBackends(MIXED, 'image_cpp').map(b => b.id);
    assert.deepStrictEqual(out, ['img-1']);
});

check('filterBackends(image_cpp) при отсутствии image-бэкендов → fallback «показать все»', function () {
    const out = F.filterBackends([{ id: 'llama-1', type: 'llama_cpp' }], 'image_cpp').map(b => b.id);
    assert.deepStrictEqual(out, ['llama-1'], 'пустой список хуже, чем неверный фильтр');
});

check('getAvailableModes(image_cpp) = standard/replication/rpc_coordinator', function () {
    assert.deepStrictEqual(F.getAvailableModes('image_cpp'), ['standard', 'replication', 'rpc_coordinator']);
    assert.strictEqual(F.isModeAvailable('virtual_router', 'image_cpp'), false);
});

check('setCurrentType(image_cpp) — валидный фильтр (localStorage + updateUI)', function () {
    let uiType = null;
    const origUpdateUI = F.updateUI;
    F.updateUI = function (t) { uiType = t; };
    try {
        F.setCurrentType('image_cpp');
        assert.strictEqual(localStorage.getItem('ollamalegion_backend_type'), 'image_cpp');
        assert.strictEqual(uiType, 'image_cpp');
        assert.strictEqual(F.getCurrentType(), 'image_cpp');
        // мусорный тип не должен менять состояние
        F.setCurrentType('bogus');
        assert.strictEqual(localStorage.getItem('ollamalegion_backend_type'), 'image_cpp');
        assert.strictEqual(uiType, 'image_cpp');
    } finally {
        F.updateUI = origUpdateUI;
    }
});

check('toggleBackendFormFields(image_cpp): виден только блок image-полей', function () {
    F.toggleBackendFormFields('image_cpp');
    assert.strictEqual(FAKE_FIELDS['.backend-field-ollama'][0].style.display, 'none');
    assert.strictEqual(FAKE_FIELDS['.backend-field-llama'][0].style.display, 'none');
    assert.strictEqual(FAKE_FIELDS['.backend-field-image'][0].style.display, '');

    F.toggleBackendFormFields('llama_cpp');
    assert.strictEqual(FAKE_FIELDS['.backend-field-llama'][0].style.display, '');
    assert.strictEqual(FAKE_FIELDS['.backend-field-image'][0].style.display, 'none');

    F.toggleBackendFormFields('ollama');
    assert.strictEqual(FAKE_FIELDS['.backend-field-ollama'][0].style.display, '');
    assert.strictEqual(FAKE_FIELDS['.backend-field-llama'][0].style.display, 'none');
    assert.strictEqual(FAKE_FIELDS['.backend-field-image'][0].style.display, 'none');
});

check('_saveToServer(image_cpp) не отправляет image_cpp как backendEngine', function () {
    let called = false;
    const origApi = window.Api;
    window.Api = { updateConfig: function () { called = true; } };
    try {
        F._saveToServer('image_cpp');
        assert.strictEqual(called, false, 'image_cpp - UI-фильтр, а не engine кластера');
    } finally {
        window.Api = origApi;
    }
});

console.log('\nOK: ' + passed + ' checks passed');
