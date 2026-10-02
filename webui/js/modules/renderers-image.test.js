// renderers-image.test.js — R-Image Phase 5 (2026-10-02): проверка отрисовки
// image-бэкенда (image_cpp) в renderers.js (страницы «Бэкенды» и «Модели»).
//
// Запуск: node webui/js/modules/renderers-image.test.js
//
// ЗАЧЕМ. Симптом пользователя: «WebUI не показывает image-бэкенд с его моделью
// и отдельным портом». Причина была в том, что UI читал ollamaPort/cppWorkerPort
// и ollama.runningModels, а image-воркер отдаёт imagePort и backend.image.models.
// Тесты фиксируют ровно это:
//   1. Колонка порта: у image-бэкенда 18093 (imagePort), а НЕ 11434 (ollamaPort).
//   2. Раскрытая строка: состояние воркера, VRAM, модели со статусом/размером и
//      предупреждение о lastError.
//   3. Страница «Модели»: image-модели попадают в modelsGrid с бейджем 🎨,
//      без bulk-checkbox (bulk load/unload идёт чужим API) и без кнопок
//      load/unload/delete (управление — на странице «Изображения»).
//   4. Регрессия: ollama/llama.cpp карточки и порты не изменились.

'use strict';

const assert = require('assert');

// --- минимальный DOM ---------------------------------------------------------
// renderers.js на этапе загрузки вешает document.addEventListener и использует
// Utils.setText/setHTML/getElementById; Utils.escapeHtml создаёт элемент.
function makeEl(id) {
    const el = {
        id: id || '',
        innerHTML: '',
        textContent: '',
        value: '',
        hidden: false,
        style: {},
        dataset: {},
        children: [],
        classList: { add() {}, remove() {}, contains() { return false; } },
        setAttribute() {},
        getAttribute() { return null; },
        appendChild() {},
        removeChild() {},
        addEventListener() {},
        querySelector() { return null; },
        querySelectorAll() { return []; },
    };
    return el;
}

const byId = {};
let navClicked = false;
global.document = {
    createElement() {
        let text = '';
        const el = makeEl('');
        Object.defineProperty(el, 'textContent', { get() { return text; }, set(v) { text = String(v); } });
        Object.defineProperty(el, 'innerHTML', {
            get() {
                return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
            },
        });
        return el;
    },
    getElementById(id) { return byId[id] || (byId[id] = makeEl(id)); },
    querySelector(sel) {
        if (sel === '[data-page="image"]') return { click() { navClicked = true; } };
        return null;
    },
    querySelectorAll() { return []; },
    addEventListener() {},
    removeEventListener() {},
};
global.window = global;

// Мини-словарь i18n: ключи, которые реально есть в en.js/ru.js. Неизвестный ключ
// возвращаем как есть — тест ловит опечатку в имени ключа (в UI был бы виден
// сырой ключ вместо текста).
const I18N_MAP = {
    'gguf.model_state_loaded': 'Loaded',
    'gguf.model_state_unloaded': 'Not loaded',
    'gguf.model_state_loading': 'Loading...',
    'gguf.model_state_error': 'Error',
    'image.section_worker': 'image.cpp worker',
    'image.worker_port': 'Worker port',
    'image.worker_state': 'Worker state',
    'image.worker_vram': 'Worker VRAM',
    'image.port_unset': 'not set',
    'image.current_model': 'Current model',
    'image.updated': 'Updated',
    'image.last_error': 'Worker error',
    'image.last_error_stale': 'The values above may be stale.',
    'image.active_badge': 'active',
    'image.col_state': 'State',
    'image.size': 'Size',
    'image.col_vram': 'VRAM estimate',
    'image.col_family': 'Family',
    'image.col_active': 'Active requests',
    'image.models_title': 'Image models',
    'image.no_models': 'No image models on this backend.',
    'image.open_page': 'Manage on the Images page',
    'backends.host': 'Host',
    'models.load': 'Load',
    'models.unload': 'Unload',
    'models.delete': 'Delete',
    'models.memory_title': 'Memory',
    'renderers.digest': 'Digest',
    'renderers.expires': 'Expires',
};
global.window.I18N = {
    t(k) { return Object.prototype.hasOwnProperty.call(I18N_MAP, k) ? I18N_MAP[k] : k; },
    getLang() { return 'en'; },
};

// Utils должен быть доступен ДО require('./renderers.js'): renderers.js
// деструктурирует его на верхнем уровне IIFE.
require('./utils.js');
global.Utils = global.window.Utils;
require('./renderers.js');
const R = global.window.Renderers;
assert.ok(R, 'window.Renderers должен быть экспортирован');

let passed = 0;
function check(name, fn) {
    fn();
    passed++;
    console.log('  \u2713 ' + name);
}

// Снимок image-бэкенда из GET /api/v1/metrics (см. описание issue).
function imageBackend() {
    return {
        id: 'image-real',
        backendType: 'image_cpp',
        status: 'healthy',
        host: '127.0.0.1',
        ollamaPort: 11434,
        cppWorkerPort: 0,
        imagePort: 18093,
        image: {
            state: 'loaded',
            currentModel: 'sd15-q4',
            vramFreeMb: 894,
            vramTotalMb: 8192,
            updatedAt: '2026-10-02T10:00:00Z',
            lastError: '',
            models: [
                { name: 'sd15-q4', state: 'loaded', family: 'sd15', sizeBytes: 1526, vramEstimateMb: 2600, activeQueries: 0 },
                { name: 'sdxl-base', state: 'not_loaded', family: 'sdxl', sizeBytes: 6800, vramEstimateMb: 7800, activeQueries: 0 },
            ],
        },
    };
}

function ollamaBackend() {
    return {
        id: 'ollama-1', backendType: 'ollama', status: 'healthy', host: '10.0.0.5', ollamaPort: 11434,
        ollama: { runningModels: [{ name: 'llama3.1', family: 'llama', parameterSize: '8B', quantization: 'Q4_K_M' }] },
    };
}

function llamaCppBackend() {
    return {
        id: 'llama-2', backendType: 'llama_cpp', status: 'healthy', host: '10.0.0.6', cppWorkerPort: 18092,
        llamaCpp: { loadedModels: [{ name: 'Qwen3.8-27B-UD-Q4_K_M', contextLength: 65536 }] },
    };
}

function renderBackendsPage(backends) {
    byId['backendsManageBody'] = makeEl('backendsManageBody');
    R.backendsPage(backends);
    return byId['backendsManageBody'].innerHTML;
}

function renderModelsPage(backends) {
    byId['modelsGrid'] = makeEl('modelsGrid');
    byId['backendLoadList'] = makeEl('backendLoadList');
    R.modelsPage(backends);
    return { grid: byId['modelsGrid'].innerHTML, load: byId['backendLoadList'].innerHTML };
}

console.log('renderers: image.cpp на страницах «Бэкенды» и «Модели»');

// --- 1. Страница «Бэкенды»: порт воркера -------------------------------------
check('Backends: image-строка показывает imagePort 18093, а НЕ ollamaPort 11434', function () {
    const html = renderBackendsPage([imageBackend()]);
    assert.ok(html.indexOf('18093') !== -1, 'imagePort должен быть в строке');
    assert.strictEqual(html.indexOf('11434'), -1, 'ollamaPort=11434 не должен показываться у image-бэкенда');
    assert.ok(html.indexOf('🎨 image.cpp') !== -1, 'бейдж типа из utils.js (без дублирования)');
});

check('Backends: imagePort=0 -> «не задан», а не 0/11434', function () {
    const b = imageBackend();
    b.imagePort = 0;
    const html = renderBackendsPage([b]);
    assert.ok(html.indexOf('not set') !== -1, 'подпись «не задан»');
    assert.strictEqual(html.indexOf('11434'), -1, 'ollamaPort не подставляется вместо imagePort');
});

// --- 2. Страница «Бэкенды»: раскрытая строка ---------------------------------
check('Backends: состояние воркера, VRAM, текущая модель', function () {
    const html = renderBackendsPage([imageBackend()]);
    assert.ok(html.indexOf('image.cpp worker') !== -1, 'секция воркера');
    assert.ok(html.indexOf('Worker state') !== -1, 'подпись состояния воркера');
    assert.ok(html.indexOf('Loaded') !== -1, 'состояние загружено');
    assert.ok(html.indexOf('894 MB / 8.0 GB') !== -1, 'VRAM воркера (свободно/всего)');
    assert.ok(html.indexOf('Current model') !== -1 && html.indexOf('sd15-q4') !== -1, 'текущая модель');
});

check('Backends: модели воркера со статусом, размером и оценкой VRAM', function () {
    const html = renderBackendsPage([imageBackend()]);
    assert.ok(html.indexOf('sdxl-base') !== -1, 'вторая модель тоже видна');
    assert.ok(html.indexOf('Not loaded') !== -1, 'статус not_loaded');
    assert.ok(html.indexOf('2.5 GB') !== -1, 'VRAM-оценка 2600 МБ -> 2.5 GB');
    assert.ok(html.indexOf('1.5 GB') !== -1, 'размер 1526 -> 1.5 GB (не 0.0 GB)');
    assert.ok(html.indexOf('active') !== -1, 'currentModel помечена активной');
});

check('Backends: lastError показывается предупреждением', function () {
    const b = imageBackend();
    b.image.lastError = 'sd-server: connection refused';
    const html = renderBackendsPage([b]);
    assert.ok(html.indexOf('Worker error') !== -1, 'подпись ошибки');
    assert.ok(html.indexOf('sd-server: connection refused') !== -1, 'текст ошибки');
    assert.ok(html.indexOf('may be stale') !== -1, 'предупреждение об устаревших данных');
});

check('Backends: без ошибки предупреждения нет', function () {
    const html = renderBackendsPage([imageBackend()]);
    assert.strictEqual(html.indexOf('Worker error'), -1);
});

check('Backends: image-бэкенд без блока image не падает', function () {
    const html = renderBackendsPage([{ id: 'image-bare', backendType: 'image_cpp', host: 'h', imagePort: 18093 }]);
    assert.ok(html.indexOf('18093') !== -1);
    assert.ok(html.indexOf('No image models on this backend.') !== -1);
});

// --- 3. Регрессия: Ollama / llama.cpp ---------------------------------------
check('Backends: llama.cpp-порт (cppWorkerPort) и Ollama-порт не изменились', function () {
    const html = renderBackendsPage([ollamaBackend(), llamaCppBackend()]);
    assert.ok(html.indexOf('18092') !== -1, 'cppWorkerPort llama.cpp');
    assert.ok(html.indexOf('11434') !== -1, 'ollamaPort у Ollama-бэкенда');
    assert.ok(html.indexOf('🦙 Ollama') !== -1 && html.indexOf('🦒 llama.cpp') !== -1, 'прежние бейджи типов');
});

// --- 4. Страница «Модели» ----------------------------------------------------
check('Models: image-модели попадают в сетку с бейджем 🎨 и типом image_cpp', function () {
    const out = renderModelsPage([imageBackend()]);
    assert.ok(out.grid.indexOf('data-backend-type="image_cpp"') !== -1, 'тип карточки image_cpp');
    assert.ok(out.grid.indexOf('🎨 image.cpp') !== -1, 'бейдж типа (тот же, что в utils.js)');
    assert.ok(out.grid.indexOf('sd15-q4') !== -1, 'загруженная модель');
    assert.ok(out.grid.indexOf('sdxl-base') !== -1, 'незагруженная модель тоже в списке');
    assert.ok(out.grid.indexOf('Not loaded') !== -1, 'состояние модели в карточке');
});

check('Models: размер и VRAM image-модели в data-атрибутах (сортировка)', function () {
    const out = renderModelsPage([imageBackend()]);
    assert.ok(out.grid.indexOf('data-vram-mb="2600"') !== -1, 'оценка VRAM для сортировки');
    assert.ok(out.grid.indexOf('data-size-bytes="' + (1526 * 1024 * 1024) + '"') !== -1, 'размер (1526 МБ) в байтах');
});

check('Models: у image-карточки нет bulk-checkbox и нет load/unload/delete', function () {
    const out = renderModelsPage([imageBackend()]);
    assert.strictEqual(out.grid.indexOf('model-select-cb'), -1,
        'bulk load/unload идёт ollama-путём бэкенда и image-воркер его не понимает');
    assert.strictEqual(out.grid.indexOf("modelCardAction('load'"), -1, 'нет ollama-кнопки load');
    assert.strictEqual(out.grid.indexOf("modelCardAction('unload'"), -1, 'нет ollama-кнопки unload');
    assert.strictEqual(out.grid.indexOf("modelCardAction('delete'"), -1, 'нет ollama-кнопки delete');
    assert.ok(out.grid.indexOf('openImagePage(') !== -1, 'вместо них — переход на страницу «Изображения»');
    assert.ok(out.grid.indexOf('Manage on the Images page') !== -1, 'подпись кнопки');
});

check('Models: блок «Загрузка бэкендов» видит image-модели, а не 0', function () {
    const out = renderModelsPage([imageBackend()]);
    assert.ok(out.load.indexOf('sd15-q4') !== -1, 'модель воркера в списке загрузки');
    assert.ok(out.load.indexOf('Loaded') !== -1, 'её состояние');
    assert.ok(out.load.indexOf('1600126976') === -1 && out.load.indexOf('1.5 GB') !== -1, 'размер 1526 -> 1.5 GB');
});

check('Models: счётчик modelsTotal учитывает image-модели', function () {
    renderModelsPage([imageBackend(), ollamaBackend()]);
    assert.strictEqual(byId['modelsTotal'].textContent, 3, '2 image-модели + 1 ollama');
});

check('Models: ollama-карточка сохранила checkbox и кнопки load/unload/delete', function () {
    const out = renderModelsPage([ollamaBackend()]);
    assert.ok(out.grid.indexOf('model-select-cb') !== -1, 'bulk-checkbox на месте');
    assert.ok(out.grid.indexOf("modelCardAction('load'") !== -1);
    assert.ok(out.grid.indexOf("modelCardAction('unload'") !== -1);
    assert.ok(out.grid.indexOf("modelCardAction('delete'") !== -1);
    assert.ok(out.grid.indexOf('🦙 Ollama') !== -1, 'бейдж типа');
});

check('Models: llama.cpp-карточка без кнопок load/unload/delete (как раньше)', function () {
    const out = renderModelsPage([llamaCppBackend()]);
    assert.ok(out.grid.indexOf('model-select-cb') !== -1);
    assert.strictEqual(out.grid.indexOf("modelCardAction('load'"), -1);
    assert.ok(out.grid.indexOf("openModelDetailsModal(") !== -1, 'ⓘ остался');
});

// --- 5. openImagePage --------------------------------------------------------
check('openImagePage: переключает вкладку на «Изображения»', function () {
    navClicked = false;
    R.openImagePage('image-real');
    assert.strictEqual(navClicked, true, 'клик по nav [data-page="image"]');
});

check('openImagePage: без backendId не падает', function () {
    R.openImagePage('');
});

console.log('\nrenderers-image: ' + passed + ' проверок пройдено');
