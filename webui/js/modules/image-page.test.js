// image-page.test.js — R-Image Phase 9 (2026-10-03): юнит-тесты чистых функций
// страницы «Image-модели» (webui/js/modules/image-page.js).
//
// Запуск: node webui/js/modules/image-page.test.js
// (модуль грузится в node: он не трогает DOM на этапе загрузки, экспортирует
//  чистые хелперы через window.ImagePage.pure).
//
// Что проверяем и почему:
//   1. parseOpenAIError — конверт ошибки балансера/воркера {error:{message,code}},
//      плоский {error,message}, сырой текст и пустой ответ: им разбираются ошибки
//      всех запросов страницы (модели, load/unload, HF).
//   2. normalizeModels — список image-моделей из ответа воркера (name/state/
//      размер/семейство/VRAM): по нему строится таблица «Модели на диске».
//   3. normalizeBackends — фильтр image_cpp при чтении /api/v1/backends.
//   4. parseBundleRows — роли/обязательные поля/дубликаты/lora-исключение
//      (контракт pkg/types/image_model.go).
//   5. aggregateDownloadProgress — общий прогресс bundle по файлам (таб HF).
//   6. normalizeLoadProgress — снимок прогресса загрузки модели.
//   7. formatBytes / formatDuration / stateLabelKey / escapeHtml.
//
// ЧЕГО ЗДЕСЬ НЕТ И ПОЧЕМУ: тестов генерации (buildGenerationPayload,
// extractImages, b64ToDataUrl, snapDimension, normalizeCapabilities) — вместе с
// формой генерации, результатом и галереей эти функции из модуля удалены
// (Phase 9: WebUI настраивает и показывает состояние, картинки показывает клиент).

'use strict';

const assert = require('assert');

// Минимальное окружение: модуль пишет window.ImagePage.
global.window = global;
require('./image-page.js');

const P = global.ImagePage && global.ImagePage.pure;
assert.ok(P, 'window.ImagePage.pure должен быть экспортирован');

let passed = 0;
function check(name, fn) {
    fn();
    passed++;
    console.log('  \u2713 ' + name);
}

console.log('image-page pure helpers');

// --- 1. parseOpenAIError ---------------------------------------------------
check('parseOpenAIError: OpenAI-конверт с message и code', function () {
    const msg = P.parseOpenAIError({ error: { message: 'no healthy backend', type: 'invalid_request_error', code: 'image_backend_unavailable' } }, 503);
    assert.strictEqual(msg, 'no healthy backend (image_backend_unavailable)');
});

check('parseOpenAIError: плоский конверт {error, message}', function () {
    assert.strictEqual(P.parseOpenAIError({ error: 'bad_request', message: 'width must be multiple of 64' }, 400), 'bad_request');
});

check('parseOpenAIError: JSON-строка, сырой текст и пустой ответ', function () {
    assert.strictEqual(P.parseOpenAIError('{"error":{"message":"boom"}}', 500), 'boom');
    assert.strictEqual(P.parseOpenAIError('gateway timeout', 504), 'gateway timeout');
    assert.strictEqual(P.parseOpenAIError('', 500), 'HTTP 500');
    assert.strictEqual(P.parseOpenAIError(null, 502), 'HTTP 502');
});

// --- 2. normalizeModels ----------------------------------------------------
check('normalizeModels: {models:[...]} — имя/состояние/размер/семейство/VRAM', function () {
    const list = P.normalizeModels({ models: [
        { name: 'sd15-q8', state: 'loaded', size_bytes: 1760000000, family: 'sd15', active_queries: 2, vram_estimate_mb: 2100, defaults: { steps: 8 } },
    ] });
    assert.strictEqual(list.length, 1);
    assert.strictEqual(list[0].name, 'sd15-q8');
    assert.strictEqual(list[0].state, 'loaded');
    assert.strictEqual(list[0].size_bytes, 1760000000);
    assert.strictEqual(list[0].family, 'sd15');
    assert.strictEqual(list[0].active_queries, 2);
    assert.strictEqual(list[0].vram_estimate_mb, 2100);
    assert.strictEqual(list[0].defaults.steps, 8);
});

check('normalizeModels: альтернативные написания (id/sizeBytes/vramEstimateMB) и мусор', function () {
    const list = P.normalizeModels([
        { id: 'flux-q4', sizeBytes: 7000000000, vramEstimateMB: 8192 },
        { name: '' },
        null,
    ]);
    assert.deepStrictEqual(list.map(m => m.name), ['flux-q4']);
    assert.strictEqual(list[0].size_bytes, 7000000000);
    assert.strictEqual(list[0].vram_estimate_mb, 8192);
    assert.strictEqual(list[0].state, 'not_loaded', 'без state модель считается незагруженной');
    assert.deepStrictEqual(P.normalizeModels(null), []);
    assert.deepStrictEqual(P.normalizeModels({}), []);
});

// --- 3. normalizeBackends --------------------------------------------------
check('normalizeBackends: /api/v1/image/backends — берём все, читаем imagePort', function () {
    const list = P.normalizeBackends({ backends: [{ id: 'img-1', name: 'GPU image', host: '10.0.0.5', imagePort: 18093, status: 'healthy' }] }, false);
    assert.strictEqual(list.length, 1);
    assert.strictEqual(list[0].id, 'img-1');
    assert.strictEqual(list[0].imagePort, 18093);
});

check('normalizeBackends: /api/v1/backends — фильтр по типу image_cpp (смешанный кластер)', function () {
    const list = P.normalizeBackends({ backends: [
        { id: 'llama-1', type: 'llama_cpp' },
        { id: 'ollama-1', type: 'ollama' },
        { id: 'img-1', type: 'image_cpp', imagePort: 18103 },
        { id: 'img-2', engine: 'image_cpp' },
        { id: '', type: 'image_cpp' },
    ] }, true);
    assert.deepStrictEqual(list.map(b => b.id), ['img-1', 'img-2']);
    assert.strictEqual(list[0].imagePort, 18103);
});

// --- 4. parseBundleRows ----------------------------------------------------
check('parseBundleRows: валидный bundle (diffusion + vae + t5xxl), пустые строки игнорируются', function () {
    const res = P.parseBundleRows([
        { role: 'diffusion', repo: 'leejet/FLUX.1-schnell-gguf', filename: 'flux1-schnell-Q4_0.gguf' },
        { role: 'vae', repo: 'black-forest-labs/FLUX.1-schnell', filename: 'ae.safetensors' },
        { role: 't5xxl', repo: 'leejet/t5', filename: 't5xxl-Q4_K_M.gguf' },
        { role: '', repo: '', filename: '' },
    ]);
    assert.deepStrictEqual(res.errors, []);
    assert.strictEqual(res.files.length, 3);
    assert.strictEqual(res.files[0].revision, 'main');
    assert.strictEqual(res.files[2].role, 't5xxl');
});

check('parseBundleRows: без diffusion — ошибка need_diffusion', function () {
    const res = P.parseBundleRows([{ role: 'vae', repo: 'r', filename: 'v.safetensors' }]);
    assert.strictEqual(res.files.length, 1);
    assert.deepStrictEqual(res.errors, [{ code: 'need_diffusion', row: 0 }]);
});

check('parseBundleRows: неизвестная роль, пустые поля и дубликат роли (кроме lora)', function () {
    const res = P.parseBundleRows([
        { role: 'diffusion', repo: 'r', filename: 'd.gguf' },
        { role: 'vae', repo: 'r', filename: '' },
        { role: 'nonsense', repo: 'r', filename: 'x.bin' },
        { role: 'diffusion', repo: 'r2', filename: 'd2.gguf' },
        { role: 'lora', repo: 'r', filename: 'l1.safetensors' },
        { role: 'lora', repo: 'r', filename: 'l2.safetensors' },
    ]);
    assert.deepStrictEqual(res.errors, [
        { code: 'missing', row: 2 },
        { code: 'role', row: 3, role: 'nonsense' },
        { code: 'dup', row: 4, role: 'diffusion' },
    ]);
    assert.strictEqual(res.files.length, 3, 'diffusion + 2 lora');
});

// --- 5. aggregateDownloadProgress -----------------------------------------
check('aggregateDownloadProgress: смешанные статусы → суммарный процент и счётчики', function () {
    const files = [
        { repo: 'a/b', filename: 'd.gguf' },
        { repo: 'a/b', filename: 'v.safetensors' },
        { repo: 'a/b', filename: 'missing.gguf' },
    ];
    const progress = {};
    progress[P.bundleFileKey(files[0])] = { status: 'downloading', downloaded: 500, totalBytes: 1000, speedBps: 2048 };
    progress[P.bundleFileKey(files[1])] = { status: 'completed', downloaded: 1000, totalBytes: 1000 };
    const agg = P.aggregateDownloadProgress(files, progress);
    assert.strictEqual(agg.total, 2000);
    assert.strictEqual(agg.downloaded, 1500);
    assert.strictEqual(agg.percent, 75);
    assert.strictEqual(agg.completed, 1);
    assert.strictEqual(agg.active, 1);
    assert.strictEqual(agg.unknown, 1);
    assert.strictEqual(agg.done, false);
});

check('aggregateDownloadProgress: все файлы завершены → 100% и done', function () {
    const files = [{ repo: 'a/b', filename: 'd.gguf' }];
    const progress = {};
    progress[P.bundleFileKey(files[0])] = { status: 'completed', downloaded: 10, totalBytes: 10 };
    const agg = P.aggregateDownloadProgress(files, progress);
    assert.strictEqual(agg.percent, 100);
    assert.strictEqual(agg.done, true);
});

check('aggregateDownloadProgress: упавший файл не считается завершённым успешно', function () {
    const files = [{ repo: 'a/b', filename: 'd.gguf' }, { repo: 'a/b', filename: 'e.gguf' }];
    const progress = {};
    progress[P.bundleFileKey(files[0])] = { status: 'completed', downloaded: 10, totalBytes: 10 };
    progress[P.bundleFileKey(files[1])] = { status: 'failed', errorMessage: 'gated' };
    const agg = P.aggregateDownloadProgress(files, progress);
    assert.strictEqual(agg.failed, 1);
    assert.strictEqual(agg.done, true);
});

// --- 6. normalizeLoadProgress ---------------------------------------------
check('normalizeLoadProgress: формат image-воркера {progress:{state,stage,elapsed_ms}}', function () {
    const snap = P.normalizeLoadProgress({
        progress: { state: 'loading', model: 'sd15-q8', stage: 'spawning sd-server', elapsed_ms: 4200, events: [] },
        state: 'loading', model: 'sd15-q8', pid: 123,
    }, 'sd15-q8');
    assert.strictEqual(snap.state, 'loading');
    assert.strictEqual(snap.stage, 'spawning sd-server');
    assert.strictEqual(snap.elapsedMs, 4200);
    assert.strictEqual(snap.model, 'sd15-q8');
    assert.ok(isNaN(snap.progressPct), 'процента нет - UI не должен рисовать полосу');
});

check('normalizeLoadProgress: зеркало cppworker {models:[...]} и чужое имя → null', function () {
    const snap = P.normalizeLoadProgress({ models: [{ name: 'sd15-q8', state: 'loaded', elapsedMs: 900 }] }, 'sd15-q8');
    assert.strictEqual(snap.state, 'loaded');
    assert.strictEqual(snap.elapsedMs, 900);
    assert.strictEqual(P.normalizeLoadProgress({ models: [{ name: 'other', state: 'loading' }] }, 'sd15-q8'), null);
    assert.strictEqual(P.normalizeLoadProgress({}, 'sd15-q8'), null);
    const withPct = P.normalizeLoadProgress({ name: 'x', state: 'loading', progressPct: 42 }, 'x');
    assert.strictEqual(withPct.progressPct, 42);
});

// --- 7. форматтеры и константы --------------------------------------------
check('formatBytes: B/KB/MB/GB, мусор → дефис', function () {
    assert.strictEqual(P.formatBytes(0), '0 B');
    assert.strictEqual(P.formatBytes(512), '512 B');
    assert.strictEqual(P.formatBytes(1024), '1.0 KB');
    assert.strictEqual(P.formatBytes(1024 * 1024 * 4.1), '4.1 MB');
    assert.strictEqual(P.formatBytes(1024 * 1024 * 1024 * 4.1), '4.1 GB');
    assert.strictEqual(P.formatBytes(null), '-');
    assert.strictEqual(P.formatBytes('abc'), '-');
});

check('formatDuration: секунды/минуты/часы', function () {
    assert.strictEqual(P.formatDuration(0), '0s');
    assert.strictEqual(P.formatDuration(9500), '9s');
    assert.strictEqual(P.formatDuration(65000), '1m 05s');
    assert.strictEqual(P.formatDuration(3720000), '1h 02m');
});

check('stateLabelKey: состояние модели → ключ i18n', function () {
    assert.strictEqual(P.stateLabelKey('loaded'), 'gguf.model_state_loaded');
    assert.strictEqual(P.stateLabelKey('loading'), 'gguf.model_state_loading');
    assert.strictEqual(P.stateLabelKey('error'), 'gguf.model_state_error');
    assert.strictEqual(P.stateLabelKey('not_loaded'), 'gguf.model_state_unloaded');
    assert.strictEqual(P.stateLabelKey(''), 'gguf.model_state_unloaded');
});

check('escapeHtml: экранирует имена моделей и файлов', function () {
    assert.strictEqual(P.escapeHtml('<img src=x onerror="alert(1)">'), '&lt;img src=x onerror=&quot;alert(1)&quot;&gt;');
    assert.strictEqual(P.escapeHtml(null), '');
});

check('IMAGE_ROLES: 12 ролей из pkg/types/image_model.go', function () {
    assert.strictEqual(P.IMAGE_ROLES.length, 12);
    ['diffusion', 'vae', 'clip_l', 'clip_g', 't5xxl', 'llm', 'clip_vision', 'taesd', 'lora', 'upscaler', 'controlnet', 'ip_adapter']
        .forEach(function (r) { assert.ok(P.IMAGE_ROLES.indexOf(r) >= 0, 'роль ' + r); });
});

check('IMAGE_FAMILIES: 12 семейств из pkg/types/image_model.go', function () {
    assert.strictEqual(P.IMAGE_FAMILIES.length, 12);
    ['sd15', 'sd21', 'sd_turbo', 'sdxl', 'sdxl_turbo', 'sd3', 'flux', 'flux2', 'chroma', 'qwen_image', 'z_image', 'other']
        .forEach(function (f) { assert.ok(P.IMAGE_FAMILIES.indexOf(f) >= 0, 'семейство ' + f); });
});

check('pure-слой не содержит генерации/галереи (удалено в Phase 9)', function () {
    ['buildGenerationPayload', 'validateGenerationForm', 'extractImages', 'b64ToDataUrl', 'snapDimension', 'clampInt', 'normalizeCapabilities', 'LIMITS', 'STATIC_SAMPLERS', 'STATIC_SCHEDULERS']
        .forEach(function (name) {
            assert.strictEqual(P[name], undefined, 'в pure не должно остаться ' + name);
        });
});

console.log('\nOK: ' + passed + ' checks passed');
