// utils-image.test.js — R-Image Phase 5 (2026-10-02): юнит-тесты чистых
// хелперов image-бэкенда из webui/js/modules/utils.js.
//
// Запуск: node webui/js/modules/utils-image.test.js
//
// ЗАЧЕМ. До этих хелперов каждая страница читала поля image-бэкенда сама, и
// расхождения были видны оператору: на странице «Бэкенды» показывался
// ollamaPort=11434 вместо imagePort (воркер слушает 18093), а список моделей
// брался из ollama.runningModels и был пустым. Тесты фиксируют:
//   1. getBackendImagePort: imagePort > image_port, мусор/0 -> 0 (не 11434).
//   2. getBackendImageModels: только image.models, иначе пустой массив.
//   3. imageModelStateKey/imageStateLabel: loaded/not_loaded/loading/error ->
//      ключи gguf.model_state_* (они уже есть в en/ru, новых ключей не нужно).
//   4. imageSizeBytes: воркер отдаёт байты, но 1526 (МБ) в поле sizeBytes -
//      иначе в карточке было бы "0.0 GB" вместо "1.5 GB".
//   5. imageVramEstimateMb / formatVramPair: "894 / 8192 MB", отсутствие -> "-".

'use strict';

const assert = require('assert');

// --- минимальный DOM: Utils.escapeHtml создаёт элемент и читает innerHTML ---
function makeFakeElement() {
    let text = '';
    const el = { id: '', style: {}, dataset: {}, children: [] };
    Object.defineProperty(el, 'textContent', { get() { return text; }, set(v) { text = String(v); } });
    Object.defineProperty(el, 'innerHTML', {
        get() {
            return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
        },
    });
    return el;
}

const byId = {};
global.document = {
    createElement() { return makeFakeElement(); },
    getElementById(id) { return byId[id] || (byId[id] = makeFakeElement()); },
};
global.window = global;

require('./utils.js');
const U = global.window.Utils;
assert.ok(U, 'window.Utils должен быть экспортирован (см. check_iife_exports.py)');
assert.ok(typeof U.getBackendImagePort === 'function', 'Utils.getBackendImagePort должен быть экспортирован');

let passed = 0;
function check(name, fn) {
    fn();
    passed++;
    console.log('  \u2713 ' + name);
}

// Живой снимок бэкенда из GET /api/v1/metrics (см. описание issue).
function imageBackend(overrides) {
    return Object.assign({
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
            ],
        },
    }, overrides || {});
}

console.log('utils image helpers');

// --- 1. тип бэкенда ---------------------------------------------------------
check('isImageBackend: image_cpp / image.cpp / sd_cpp -> true, llama_cpp -> false', function () {
    assert.strictEqual(U.isImageBackend(imageBackend()), true);
    assert.strictEqual(U.isImageBackend({ backendType: 'image.cpp' }), true);
    assert.strictEqual(U.isImageBackend({ type: 'sd_cpp' }), true);
    assert.strictEqual(U.isImageBackend({ backendType: 'llama_cpp' }), false);
    assert.strictEqual(U.isImageBackend({}), false);
});

// --- 2. порт воркера -------------------------------------------------------
check('getBackendImagePort: imagePort (camelCase) = 18093, НЕ ollamaPort 11434', function () {
    assert.strictEqual(U.getBackendImagePort(imageBackend()), 18093);
});

check('getBackendImagePort: image_port (snake_case) поддерживается', function () {
    assert.strictEqual(U.getBackendImagePort({ image_port: 18103 }), 18103);
});

check('getBackendImagePort: 0/мусор/отсутствие -> 0 (показываем «не задан», а не 11434)', function () {
    assert.strictEqual(U.getBackendImagePort({ imagePort: 0, ollamaPort: 11434 }), 0);
    assert.strictEqual(U.getBackendImagePort({ imagePort: 'abc' }), 0);
    assert.strictEqual(U.getBackendImagePort({ ollamaPort: 11434 }), 0);
    assert.strictEqual(U.getBackendImagePort(null), 0);
});

// --- 3. модели -------------------------------------------------------------
check('getBackendImageModels: берём image.models, у Ollama-бэкенда -> []', function () {
    const models = U.getBackendImageModels(imageBackend());
    assert.strictEqual(models.length, 1);
    assert.strictEqual(models[0].name, 'sd15-q4');
    assert.deepStrictEqual(U.getBackendImageModels({ ollama: { runningModels: [{ name: 'llama3.1' }] } }), []);
    assert.deepStrictEqual(U.getBackendImageModels({ image: {} }), []);
});

check('getBackendImage: состояние/ошибка читаются из backend.image', function () {
    assert.strictEqual(U.getBackendImageState(imageBackend()), 'loaded');
    assert.strictEqual(U.getBackendImageLastError(imageBackend()), '');
    assert.strictEqual(U.getBackendImageState(imageBackend({ image: { state: 'ERROR', lastError: 'boom' } })), 'error');
    assert.strictEqual(U.getBackendImageLastError(imageBackend({ image: { lastError: 'boom' } })), 'boom');
    assert.strictEqual(U.getBackendImage(null), null);
});

// --- 4. состояние -> ключи i18n -------------------------------------------
check('imageModelStateKey: loaded/not_loaded/loading/error -> gguf.model_state_*', function () {
    assert.strictEqual(U.imageModelStateKey('loaded'), 'gguf.model_state_loaded');
    assert.strictEqual(U.imageModelStateKey('not_loaded'), 'gguf.model_state_unloaded');
    assert.strictEqual(U.imageModelStateKey('loading'), 'gguf.model_state_loading');
    assert.strictEqual(U.imageModelStateKey('error'), 'gguf.model_state_error');
    assert.strictEqual(U.imageModelStateKey('failed'), 'gguf.model_state_error');
    assert.strictEqual(U.imageModelStateKey(''), 'gguf.model_state_unloaded');
});

check('imageStateLabel: без i18n отдаёт состояние, с i18n — перевод', function () {
    assert.strictEqual(U.imageStateLabel('loaded'), 'loaded');
    const prev = global.window.I18N;
    global.window.I18N = { t: function (k) { return k === 'gguf.model_state_loaded' ? 'Загружена' : k; } };
    try {
        assert.strictEqual(U.imageStateLabel('loaded'), 'Загружена');
        // Неизвестный ключ (i18n без перевода) -> состояние, а не имя ключа.
        assert.strictEqual(U.imageStateLabel('weird'), 'weird');
    } finally {
        global.window.I18N = prev;
    }
});

check('imageStateBadge: loaded -> badge-success, error -> badge-danger', function () {
    assert.ok(U.imageStateBadge('loaded').indexOf('badge-success') !== -1, 'loaded = success');
    assert.ok(U.imageStateBadge('loading').indexOf('badge-warning') !== -1, 'loading = warning');
    assert.ok(U.imageStateBadge('error').indexOf('badge-danger') !== -1, 'error = danger');
    assert.ok(U.imageStateBadge('not_loaded').indexOf('badge-info') !== -1, 'not_loaded = info');
});

// --- 5. размер / VRAM -----------------------------------------------------
check('imageSizeBytes: 1526 (МБ в поле sizeBytes) -> ~1.5 ГБ в байтах', function () {
    const bytes = U.imageSizeBytes({ sizeBytes: 1526 });
    assert.strictEqual(bytes, 1526 * 1024 * 1024);
    assert.strictEqual(U.formatMB(bytes / 1024 / 1024), '1.5 GB');
});

check('imageSizeBytes: настоящие байты и size_bytes не портятся', function () {
    assert.strictEqual(U.imageSizeBytes({ sizeBytes: 2 * 1024 * 1024 * 1024 }), 2147483648);
    assert.strictEqual(U.imageSizeBytes({ size_bytes: 3 * 1024 * 1024 * 1024 }), 3221225472);
    assert.strictEqual(U.imageSizeBytes({}), 0);
    assert.strictEqual(U.imageSizeBytes(null), 0);
});

check('imageVramEstimateMb: 2600, snake_case и отсутствие', function () {
    assert.strictEqual(U.imageVramEstimateMb({ vramEstimateMb: 2600 }), 2600);
    assert.strictEqual(U.imageVramEstimateMb({ vram_estimate_mb: 512 }), 512);
    assert.strictEqual(U.imageVramEstimateMb({}), 0);
});

check('formatVramPair: "894 MB / 8.0 GB", отсутствие -> "-"', function () {
    assert.strictEqual(U.formatVramPair(894, 8192), '894 MB / 8.0 GB');
    assert.strictEqual(U.formatVramPair(undefined, undefined), '-');
    assert.strictEqual(U.formatVramPair(0, 8192), '0 MB / 8.0 GB');
    assert.strictEqual(U.formatVramPair(null, 8192), '? / 8.0 GB');
});

console.log('\nutils-image: ' + passed + ' проверок пройдено');
