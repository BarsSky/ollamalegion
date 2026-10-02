// image-page.test.js — R-Image Phase 5 (2026-10-02): юнит-тесты чистых функций
// страницы «Изображения» (webui/js/modules/image-page.js).
//
// Запуск: node webui/js/modules/image-page.test.js
// (модуль грузится в node: он не трогает DOM на этапе загрузки, экспортирует
//  чистые хелперы через window.ImagePage.pure).
//
// Что проверяем и почему:
//   1. buildGenerationPayload — точное соответствие контракту OpenAI-тела
//      {prompt,negative_prompt,width,height,steps,cfg_scale,seed,batch_size,
//       sampler_name,scheduler}; пустой seed должен стать -1 (иначе sd.cpp
//      отдаёт одинаковые картинки: seed=42 из default_gen_params).
//   2. snapDimension — кратность 64 и границы 64..4096 (движок падает на
//      некратных размерах, сервер вернул бы 400).
//   3. parseOpenAIError — OpenAI-конверт {error:{message,type,code}},
//      плоский {error,message}, сырой текст и пустой ответ.
//   4. b64ToDataUrl — стабильный data URL, терпимость к переводам строк и
//      к уже готовому префиксу data:.
//   5. extractImages — {data:[{b64_json}]} и альтернативные формы.
//   6. normalizeCapabilities — лимиты движка в snake_case + статический
//      fallback самплеров/шедулеров.
//   7. normalizeBackends — фильтр image_cpp при чтении /api/v1/backends.
//   8. parseBundleRows — роли/обязательные поля/дубликаты/lora-исключение.
//   9. aggregateDownloadProgress — общий прогресс bundle по файлам.
//  10. formatBytes / formatDuration / stateLabelKey.

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

// --- 1. buildGenerationPayload ---------------------------------------------
check('buildGenerationPayload: полная форма → тело OpenAI + seed/батч/сэмплер', function () {
    const body = P.buildGenerationPayload({
        prompt: '  a cat  ',
        negative: 'blurry',
        width: 513,
        height: 700,
        steps: 12,
        cfg: '1.5',
        sampler: 'euler',
        scheduler: 'karras',
        seed: '42',
        batch: 2,
    });
    assert.strictEqual(body.prompt, 'a cat');
    assert.strictEqual(body.negative_prompt, 'blurry');
    assert.strictEqual(body.width, 512, '513 → 512 (сетка 64)');
    assert.strictEqual(body.height, 704, '700 → 704 (сетка 64)');
    assert.strictEqual(body.steps, 12);
    assert.strictEqual(body.cfg_scale, 1.5);
    assert.strictEqual(body.sampler_name, 'euler');
    assert.strictEqual(body.scheduler, 'karras');
    assert.strictEqual(body.seed, 42);
    assert.strictEqual(body.batch_size, 2);
});

check('buildGenerationPayload: пустой seed → -1 (random), пустой cfg не отправляется', function () {
    const body = P.buildGenerationPayload({ prompt: 'x', width: '', height: '', steps: '', cfg: '', seed: '', batch: '' });
    assert.strictEqual(body.seed, -1, 'пусто = случайный = -1');
    assert.strictEqual(body.width, 512);
    assert.strictEqual(body.height, 512);
    assert.strictEqual(body.steps, 20, 'дефолт шагов');
    assert.strictEqual(body.batch_size, 1);
    assert.ok(!('cfg_scale' in body), 'пустой cfg = «взять дефолт профиля», а не 0');
    assert.ok(!('negative_prompt' in body), 'пустой negative не отправляем');
});

check('buildGenerationPayload: cfg 0 отправляется как 0 (валидное значение)', function () {
    const body = P.buildGenerationPayload({ prompt: 'x', cfg: '0', seed: '1' });
    assert.strictEqual(body.cfg_scale, 0);
});

check('buildGenerationPayload: значения за границами клампятся (steps 1..100, batch 1..8, cfg 0..30)', function () {
    const body = P.buildGenerationPayload({ prompt: 'x', steps: 500, batch: 99, cfg: '99' });
    assert.strictEqual(body.steps, 100);
    assert.strictEqual(body.batch_size, 8);
    assert.strictEqual(body.cfg_scale, 30);
});

// --- 2. snapDimension ------------------------------------------------------
check('snapDimension: сетка 64, границы 64..4096, мусор → 512', function () {
    assert.strictEqual(P.snapDimension(64), 64);
    assert.strictEqual(P.snapDimension(65), 64, '65 → 64 (ближайшее кратное)');
    assert.strictEqual(P.snapDimension(128), 128);
    assert.strictEqual(P.snapDimension(96), 128, '96 не кратно 64 → округление к ближайшему (128)');
    assert.strictEqual(P.snapDimension(1), 64);
    assert.strictEqual(P.snapDimension(99999), 4096);
    assert.strictEqual(P.snapDimension('abc'), 512);
    assert.strictEqual(P.snapDimension(undefined), 512);
});

// --- 3. parseOpenAIError ---------------------------------------------------
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

// --- 4. b64ToDataUrl -------------------------------------------------------
check('b64ToDataUrl: PNG по умолчанию, переводы строк вырезаются', function () {
    assert.strictEqual(P.b64ToDataUrl('iVBORw0KGgo=', 'png'), 'data:image/png;base64,iVBORw0KGgo=');
    assert.strictEqual(P.b64ToDataUrl('iVBO\nRw0K\r\nGgo='), 'data:image/png;base64,iVBORw0KGgo=');
});

check('b64ToDataUrl: jpg → image/jpeg, готовый data: не дублируется', function () {
    assert.strictEqual(P.b64ToDataUrl('abc', 'jpg'), 'data:image/jpeg;base64,abc');
    assert.strictEqual(P.b64ToDataUrl('data:image/webp;base64,zzz', 'png'), 'data:image/webp;base64,zzz');
    assert.strictEqual(P.b64ToDataUrl('', 'png'), '');
    assert.strictEqual(P.b64ToDataUrl(null, 'png'), '');
});

// --- 5. extractImages ------------------------------------------------------
check('extractImages: {data:[{b64_json}]} — сколько вернулось, столько и картинок', function () {
    const imgs = P.extractImages({ created: 1, data: [{ b64_json: 'AAA' }, { b64_json: 'BBB' }] });
    assert.strictEqual(imgs.length, 2);
    assert.strictEqual(imgs[0].b64, 'AAA');
    assert.strictEqual(imgs[1].format, 'png');
});

check('extractImages: {images:[...]}, url-вариант и пустой ответ', function () {
    assert.strictEqual(P.extractImages({ images: [{ b64_json: 'X', output_format: 'webp' }] })[0].format, 'webp');
    assert.strictEqual(P.extractImages({ data: [{ url: 'http://x/1.png' }] })[0].url, 'http://x/1.png');
    assert.deepStrictEqual(P.extractImages({}), []);
    assert.deepStrictEqual(P.extractImages(null), []);
});

// --- 6. normalizeCapabilities ---------------------------------------------
check('normalizeCapabilities: берёт samplers/schedulers/limits движка (snake_case)', function () {
    const caps = P.normalizeCapabilities({
        samplers: ['euler', 'dpm++2m'],
        schedulers: ['karras'],
        limits: { min_width: 128, max_width: 2048, min_height: 128, max_height: 2048, max_batch_count: 4 },
    });
    assert.strictEqual(caps.source, 'engine');
    assert.deepStrictEqual(caps.samplers, ['euler', 'dpm++2m']);
    assert.strictEqual(caps.limits.maxSide, 2048);
    assert.strictEqual(caps.limits.minSide, 128);
    assert.strictEqual(caps.limits.maxBatch, 4);
});

check('normalizeCapabilities: нет данных → статический fallback с source=static', function () {
    const caps = P.normalizeCapabilities(null);
    assert.strictEqual(caps.source, 'static');
    assert.ok(caps.samplers.indexOf('euler') >= 0);
    assert.ok(caps.schedulers.indexOf('karras') >= 0);
    assert.strictEqual(caps.limits.maxSide, 4096);
    assert.strictEqual(caps.limits.maxBatch, 8);
});

check('normalizeCapabilities: вложенный блок capabilities тоже понимается', function () {
    const caps = P.normalizeCapabilities({ capabilities: { samplers: ['lcm'], schedulers: ['gits'] } });
    assert.strictEqual(caps.source, 'engine');
    assert.deepStrictEqual(caps.samplers, ['lcm']);
});

// --- 7. normalizeBackends --------------------------------------------------
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

// --- 8. parseBundleRows ----------------------------------------------------
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

// --- 9. aggregateDownloadProgress -----------------------------------------
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

// --- 10. форматтеры -------------------------------------------------------
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

check('escapeHtml: экранирует prompt и имена файлов', function () {
    assert.strictEqual(P.escapeHtml('<img src=x onerror="alert(1)">'), '&lt;img src=x onerror=&quot;alert(1)&quot;&gt;');
    assert.strictEqual(P.escapeHtml(null), '');
});

check('IMAGE_ROLES: 12 ролей из pkg/types/image_model.go', function () {
    assert.strictEqual(P.IMAGE_ROLES.length, 12);
    ['diffusion', 'vae', 'clip_l', 'clip_g', 't5xxl', 'llm', 'clip_vision', 'taesd', 'lora', 'upscaler', 'controlnet', 'ip_adapter']
        .forEach(function (r) { assert.ok(P.IMAGE_ROLES.indexOf(r) >= 0, 'роль ' + r); });
});

console.log('\nOK: ' + passed + ' checks passed');
