// backend-type-filter-r112.test.js — R112 (2026-10-11).
//
// Запуск: node webui/js/modules/backend-type-filter-r112.test.js
//
// ЖИВАЯ ЖАЛОБА. «Убрать разделение по типу бэкенда — llama и ollama должны
// работать вместе, поэтому поменять параметры инициализации и маркеры в главном
// меню, показывающие, какой тип бэкенда используется».
//
// ЧТО БЫЛО НЕ ТАК:
//   1. маркер в сайдбаре называл ОДИН тип даже когда в кластере жили оба
//      (llama.cpp + Ollama) — вторая половина стенда была невидима;
//   2. серверный backendEngine=auto («доминирующего типа нет») считался
//      состоянием «сервер не знает тип», и пользовательский фильтр
//      llama.cpp/Ollama сбрасывался на «Все» каждые 2 секунды (cluster update);
//   3. getAvailableModes() ограничивал режимы по типу бэкенда, хотя сервер
//      (types.ModeBackendTypes) больше не связывает их.
//
// Окружение: минимальный DOM/localStorage-мок (как в r94-тесте).

'use strict';

const assert = require('assert');

global.window = global;

const store = {};
global.localStorage = {
    getItem: function (k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem: function (k, v) { store[k] = String(v); },
    removeItem: function (k) { delete store[k]; },
};

function fakeNav(page) {
    return {
        dataset: { page: page },
        style: { display: 'none' },
        title: '',
        classList: { add() {}, remove() {}, contains() { return false; } },
    };
}
const navAll = [fakeNav('dashboard'), fakeNav('gguf'), fakeNav('image'), fakeNav('agents')];

const badgeIcon = { textContent: '' };
const badgeClasses = [];
const badgeAttrs = {};
const badge = {
    classList: {
        add: function (c) { badgeClasses.push(c); },
        remove: function () { badgeClasses.length = 0; },
        contains: function (c) { return badgeClasses.indexOf(c) >= 0; },
    },
    querySelector: function (sel) { return sel === '.engine-icon' ? badgeIcon : null; },
    setAttribute: function (k, v) { badgeAttrs[k] = v; },
    title: '',
};
const badgeLabel = { textContent: '' };

global.document = {
    querySelectorAll: function (sel) {
        if (sel === '.nav-item[data-page]') return navAll;
        return [];
    },
    querySelector: function () { return null; },
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
    badgeClasses.length = 0;
    badgeLabel.textContent = 'Автоопределение...';
    badge.title = '';
    F._clusterTypeCounts = null;
    F._serverEngineType = '';
    delete store['ollamalegion_backend_type'];
}

// Смешанный текстовый стенд: cppworker (llama.cpp) + Ollama-нода.
const MIXED_TEXT = {
    backendEngine: 'auto',
    effectiveBackendType: '',
    backendTypeCounts: { llama_cpp: 1, ollama: 1 },
};
const MIXED_ALL = {
    backendEngine: 'auto',
    effectiveBackendType: '',
    backendTypeCounts: { llama_cpp: 1, ollama: 1, image_cpp: 1 },
};

console.log('backend-type-filter (R112: единый тип бэкенда, состав кластера в маркере)');

check('clusterComposition(): состав кластера называется полностью', function () {
    reset();
    F._clusterTypeCounts = { llama_cpp: 2, ollama: 1 };
    const comp = F.clusterComposition();
    assert.strictEqual(comp.text, 'llama.cpp + Ollama');
    assert.strictEqual(comp.icon, '🦒🦙');
    assert.strictEqual(comp.items.length, 2);
});

check('маркер при llama.cpp + Ollama называет ОБА типа (а не только llama.cpp)', function () {
    reset();
    F._serverEngineType = 'auto';
    F._clusterTypeCounts = MIXED_TEXT.backendTypeCounts;
    F.applyChrome();
    assert.strictEqual(badgeLabel.textContent, 'llama.cpp + Ollama',
        'маркер скрыл половину стенда: ' + badgeLabel.textContent);
    assert.ok(badgeClasses.indexOf('engine-mixed') >= 0, 'маркер должен быть в состоянии mixed');
    assert.strictEqual(badgeAttrs['data-cluster-composition'], 'llama.cpp + Ollama');
});

check('маркер учитывает и image-бэкенды вместе с текстовыми', function () {
    reset();
    F._serverEngineType = 'auto';
    F._clusterTypeCounts = MIXED_ALL.backendTypeCounts;
    F.applyChrome();
    assert.strictEqual(badgeLabel.textContent, 'llama.cpp + Ollama + image.cpp');
});

check('пользовательский фильтр по-прежнему подтверждается маркером', function () {
    reset();
    F._clusterTypeCounts = MIXED_TEXT.backendTypeCounts;
    F.updateEngineBadge('ollama', true);
    assert.strictEqual(badgeLabel.textContent, 'Ollama API',
        'выбор фильтра «Ollama» не подтверждён: ' + badgeLabel.textContent);
    F.updateEngineBadge('llama_cpp', true);
    assert.strictEqual(badgeLabel.textContent, 'llama.cpp');
});

check('«авто» с сервера = UI-фильтр «Все», а не «Ollama» по умолчанию', function () {
    reset();
    assert.strictEqual(F._engineToType('auto'), 'all');
    assert.strictEqual(F._engineToType('ollama_api'), 'ollama');
    // Каноническая запись «Все» — пустая строка (Round 18g).
    F._storeType('all');
    assert.strictEqual(localStorage.getItem('ollamalegion_backend_type'), '');
});

check('смешанный кластер НЕ сбрасывает пользовательский фильтр на «Все»', function () {
    reset();
    store['ollamalegion_backend_type'] = 'llama_cpp';
    const ret = F.syncFromClusterState(MIXED_TEXT);
    assert.strictEqual(ret, 'llama_cpp', 'фильтр пользователя перезаписан серверным auto');
    assert.strictEqual(localStorage.getItem('ollamalegion_backend_type'), 'llama_cpp');
});

check('без пользовательского фильтра смешанный кластер показывает «Все»', function () {
    reset();
    const ret = F.syncFromClusterState(MIXED_TEXT);
    assert.strictEqual(ret, 'all');
    assert.strictEqual(localStorage.getItem('ollamalegion_backend_type'), '');
});

check('getAvailableModes(): режим и тип бэкенда развязаны', function () {
    reset();
    ['llama_cpp', 'ollama', 'all', 'image_cpp'].forEach(function (t) {
        const modes = F.getAvailableModes(t);
        assert.ok(modes.indexOf('replication') >= 0, t + ': нет replication');
        assert.ok(modes.indexOf('virtual_router') >= 0, t + ': нет virtual_router');
    });
});

check('клик «llama.cpp» не «прыгает» на состав кластера через 2 с (cluster update)', function () {
    reset();
    store['ollamalegion_backend_type'] = 'llama_cpp';
    F._serverEngineType = 'auto';
    F._clusterTypeCounts = MIXED_TEXT.backendTypeCounts;
    F.applyChrome();
    assert.strictEqual(badgeLabel.textContent, 'llama.cpp',
        'после cluster update выбор пользователя выглядит сброшенным: ' + badgeLabel.textContent);
});

check('«Все» на смешанном стенде показывает состав кластера', function () {
    reset();
    store['ollamalegion_backend_type'] = '';
    F._serverEngineType = 'auto';
    F._clusterTypeCounts = MIXED_TEXT.backendTypeCounts;
    F.applyChrome();
    assert.strictEqual(badgeLabel.textContent, 'llama.cpp + Ollama');
});

console.log('\nOK: ' + checks + ' checks passed');
