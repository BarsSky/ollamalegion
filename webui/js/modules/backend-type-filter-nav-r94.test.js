// backend-type-filter-nav-r94.test.js — R94 (2026-10-09).
//
// Запуск: node webui/js/modules/backend-type-filter-nav-r94.test.js
//
// ЖИВАЯ ЖАЛОБА. «Пропала определённость: не может определить правильно от
// балансера WEBUI, в каком он режиме, и следовательно не отображает gguf models
// страницу» (скриншот: сайдбар без «GGUF models», бейдж «Автоопределение…»).
//
// ПРИЧИНЫ (обе исправлены):
//   1. settings-ui.js:updateVariantSections() держал СВОЙ список nav-страниц, в
//      котором не было 'gguf' и 'image', и прятал всё, чего в списке нет — то есть
//      страницы движка исчезали из сайдбара при каждом рендере режима;
//   2. BackendTypeFilter.toggleGgufTab(type) показывал страницу GGUF только при
//      type === 'llama_cpp', а syncFromClusterState для localStorage '' («Все») и
//      'image_cpp' выходит раньше и обратно её не показывал; бейдж режима в этом
//      случае оставался «Автоопределение…».
//
// НОВЫЙ КОНТРАКТ: видимость engine-страниц определяется СОСТАВОМ КЛАСТЕРА
// (backendTypeCounts от балансера), а бейдж режима — серверным
// backendEngine/effectiveBackendType; «Все» и «image.cpp» больше ничего не прячут.
//
// Окружение: минимальный DOM/localStorage-мок (как в backend-type-filter.test.js).

'use strict';

const assert = require('assert');

global.window = global;

const store = {};
global.localStorage = {
    getItem: function (k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem: function (k, v) { store[k] = String(v); },
    removeItem: function (k) { delete store[k]; },
};

// --- минимальный DOM: два nav-пункта + бейдж режима -------------------------
function fakeNav(page) {
    return {
        dataset: { page: page },
        style: { display: 'none' }, // как после updateVariantSections в баге
        title: '',
        classList: { add() {}, remove() {}, contains() { return false; } },
    };
}
const navGguf = fakeNav('gguf');
const navImage = fakeNav('image');
const navAgents = fakeNav('agents');
const navDashboard = fakeNav('dashboard');
const navAll = [navDashboard, navGguf, navImage, navAgents];

const badgeIcon = { textContent: '' };
const badge = {
    classList: { add() {}, remove() {}, contains() { return false; } },
    querySelector: function (sel) { return sel === '.engine-icon' ? badgeIcon : null; },
    title: '',
};
const badgeLabel = { textContent: '' };

global.document = {
    querySelectorAll: function (sel) {
        if (sel === '.nav-item[data-page]') return navAll;
        return [];
    },
    querySelector: function (sel) {
        if (sel === '.nav-item[data-page="gguf"]') return navGguf;
        if (sel === '.nav-item[data-page="image"]') return navImage;
        if (sel === '.nav-item[data-page="agents"]') return navAgents;
        return null;
    },
    getElementById: function (id) {
        if (id === 'backendEngineBadge') return badge;
        if (id === 'backendEngineLabel') return badgeLabel;
        return null;
    },
    addEventListener() {},
};
global.I18N = { t: function (key) { return key; } };

require('./backend-type-filter.js');
const F = global.BackendTypeFilter;
assert.ok(F, 'window.BackendTypeFilter должен быть экспортирован');

let checks = 0;
function check(name, fn) {
    fn();
    checks++;
    console.log('  \u2713 ' + name);
}
function reset() {
    navGguf.style.display = 'none';
    navImage.style.display = 'none';
    badgeLabel.textContent = 'Автоопределение...';
}
const MIXED = { backendEngine: 'llama_cpp', effectiveBackendType: 'llama_cpp', backendTypeCounts: { llama_cpp: 2, image_cpp: 2, ollama: 1 } };
const TEXT_ONLY = { backendEngine: 'llama_cpp', effectiveBackendType: 'llama_cpp', backendTypeCounts: { llama_cpp: 2 } };
const IMAGE_ONLY = { backendEngine: 'image_cpp', effectiveBackendType: 'image_cpp', backendTypeCounts: { image_cpp: 2 } };

console.log('backend-type-filter (engine pages vs cluster composition)');

check('«Все» (localStorage="") при смешанном кластере: GGUF-страница остаётся', function () {
    reset();
    store['ollamalegion_backend_type'] = '';
    F.syncFromClusterState(MIXED);
    assert.notStrictEqual(navGguf.style.display, 'none',
        'страница GGUF снова спрятана, хотя в кластере есть llama.cpp — ровно жалоба оператора');
});

check('«image.cpp» (localStorage="image_cpp"): GGUF-страница тоже остаётся', function () {
    reset();
    store['ollamalegion_backend_type'] = 'image_cpp';
    F.syncFromClusterState(MIXED);
    assert.notStrictEqual(navGguf.style.display, 'none',
        'выбор фильтра «image.cpp» не должен делать страницу GGUF недостижимой');
    assert.notStrictEqual(navImage.style.display, 'none', 'страница Image-моделей обязана быть видна');
});

check('бейдж режима не остаётся «Автоопределение…», а называет режим', function () {
    reset();
    store['ollamalegion_backend_type'] = '';
    F.syncFromClusterState(MIXED);
    assert.ok(badgeLabel.textContent && badgeLabel.textContent.indexOf('Автоопределение') === -1,
        'бейдж режима снова неопределённый: ' + badgeLabel.textContent);
    assert.ok(badgeLabel.textContent.indexOf('llama.cpp') !== -1,
        'в смешанном кластере режим должен называть текстовый движок: ' + badgeLabel.textContent);
});

check('чисто текстовый кластер: GGUF есть, Image-моделей нет', function () {
    reset();
    store['ollamalegion_backend_type'] = 'llama_cpp';
    F.syncFromClusterState(TEXT_ONLY);
    assert.notStrictEqual(navGguf.style.display, 'none', 'GGUF обязан быть виден');
    assert.strictEqual(navImage.style.display, 'none', 'на чисто текстовом стенде image-страница не нужна');
});

check('чисто image-кластер: GGUF скрыт, Image-модели видны', function () {
    reset();
    store['ollamalegion_backend_type'] = 'image_cpp';
    F.syncFromClusterState(IMAGE_ONLY);
    assert.strictEqual(navGguf.style.display, 'none', 'на чисто image-стенде страница GGUF не нужна');
    assert.notStrictEqual(navImage.style.display, 'none', 'страница Image-моделей обязана быть видна');
});

check('updateEngineBadge("") больше не даёт «Автоопределение…»', function () {
    reset();
    F._serverEngineType = 'llama_cpp';
    F._clusterTypeCounts = { llama_cpp: 1 };
    F.updateEngineBadge('');
    assert.ok(badgeLabel.textContent.indexOf('Автоопределение') === -1,
        'пустой тип снова оставил бейдж неопределённым: ' + badgeLabel.textContent);
    assert.strictEqual(badgeLabel.textContent, 'llama.cpp');
});

console.log('\nOK: ' + checks + ' checks passed');
