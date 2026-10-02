// image-profiles.test.js - R-Image Phase 6 (2026-10-02): юнит-тесты чистых
// функций редактора профилей image-моделей (webui/js/modules/image-profiles.js).
//
// Запуск: node webui/js/modules/image-profiles.test.js
// (в этом окружении `node --test` падает на spawn EPERM - известное ограничение
//  песочницы, поэтому тест запускается файлом, как остальные JS-тесты проекта.)
//
// Что проверяем и почему:
//   1. profileToForm - разворачивание вложенных defaults/runtime в плоскую
//      форму; без этого форма читала бы undefined и теряла поля профиля.
//   2. formToProfile - сборка тела PUT: числа, булевы (они должны уходить
//      ВСЕГДА, иначе сервер не различит false и «не передали»), extraArgs,
//      сохранение localPath/sizeBytes у неизменившихся строк файлов.
//   3. validateForm - зеркало types.ValidateImageModelProfile
//      (pkg/types/image_model.go:225-317): имя, diffusion, дубли ролей,
//      диапазоны, кратность 64, пара offload+paramsBackend, VAE для DiT.
//   4. normalizeApplyResponse - честный разбор apply: persisted/unreachable/
//      partial/running и вывод outcome (нельзя показать «успех» при
//      недоступном воркере).
//   5. normalizeProfileList / normalizeCatalog / normalizeBackends / parseApiError.
//   6. familyDefaultsFor / bundleSizeBytes / formatBytes / formatDuration.

'use strict';

const assert = require('assert');

// Модуль пишет window.ImageProfiles и на загрузке не трогает DOM.
global.window = global;
require('./image-profiles.js');

const IP = global.ImageProfiles;
assert.ok(IP, 'window.ImageProfiles должен быть экспортирован');
const P = IP.pure;
assert.ok(P, 'window.ImageProfiles.pure должен быть экспортирован');

let passed = 0;
const failures = [];
function check(name, fn) {
    try {
        fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' - ' + (e && e.message));
    }
}

console.log('image-profiles pure helpers');

// --- 1. profileToForm ------------------------------------------------------
const FULL_PROFILE = {
    name: 'flux-schnell-q4',
    family: 'flux',
    disabled: true,
    notes: 'test note',
    vramEstimateMb: 7000,
    timeoutSec: 600,
    idleUnloadMinutes: 15,
    files: [
        { role: 'diffusion', repo: 'leejet/FLUX.1-schnell-gguf', filename: 'flux1-schnell-Q4_0.gguf', revision: 'main', sizeBytes: 6800000000, localPath: '/models/flux/q4.gguf' },
        { role: 'vae', repo: 'black-forest-labs/FLUX.1-schnell', filename: 'ae.safetensors' },
        { role: 'lora', repo: 'r/l1', filename: 'a.safetensors' },
        { role: 'lora', repo: 'r/l2', filename: 'b.safetensors' }
    ],
    defaults: { steps: 4, cfgScale: 1, sampler: 'euler', scheduler: 'simple', width: 1024, height: 768, batchCount: 2, negativePrompt: 'blurry', seed: 42, clipSkip: 2 },
    runtime: {
        backend: 'te=cpu', paramsBackend: 'disk', maxVram: '6', offloadToCpu: false,
        autoFit: 'on', diffusionFa: true, vaeTiling: true, vaeTileSize: 512,
        vaeConvDirect: true, taesd: true, splitMode: 'row', nGpuLayers: 24,
        threads: 6, seedMode: 'fixed', extraArgs: ['--vae-tile-overlap', '0.25']
    }
};

check('profileToForm: вложенные defaults/runtime разворачиваются в плоскую форму', function () {
    const f = P.profileToForm(FULL_PROFILE, true);
    assert.strictEqual(f.name, 'flux-schnell-q4');
    assert.strictEqual(f.family, 'flux');
    assert.strictEqual(f.disabled, true);
    assert.strictEqual(f.vramEstimateMb, 7000);
    assert.strictEqual(f.timeoutSec, 600);
    assert.strictEqual(f.idleUnloadMinutes, 15);
    assert.strictEqual(f.defaultsSteps, 4);
    assert.strictEqual(f.defaultsCfgScale, 1);
    assert.strictEqual(f.defaultsSampler, 'euler');
    assert.strictEqual(f.defaultsScheduler, 'simple');
    assert.strictEqual(f.defaultsWidth, 1024);
    assert.strictEqual(f.defaultsHeight, 768);
    assert.strictEqual(f.defaultsBatchCount, 2);
    assert.strictEqual(f.defaultsNegativePrompt, 'blurry');
    assert.strictEqual(f.defaultsSeed, 42);
    assert.strictEqual(f.defaultsClipSkip, 2);
    assert.strictEqual(f.runtimeBackend, 'te=cpu');
    assert.strictEqual(f.runtimeParamsBackend, 'disk');
    assert.strictEqual(f.runtimeMaxVram, '6');
    assert.strictEqual(f.runtimeOffloadToCpu, false);
    assert.strictEqual(f.runtimeAutoFit, 'on');
    assert.strictEqual(f.runtimeDiffusionFa, true);
    assert.strictEqual(f.runtimeVaeTiling, true);
    assert.strictEqual(f.runtimeVaeTileSize, 512);
    assert.strictEqual(f.runtimeVaeConvDirect, true);
    assert.strictEqual(f.runtimeTaesd, true);
    assert.strictEqual(f.runtimeSplitMode, 'row');
    assert.strictEqual(f.runtimeNGpuLayers, 24);
    assert.strictEqual(f.runtimeThreads, 6);
    assert.strictEqual(f.runtimeSeedMode, 'fixed');
    assert.deepStrictEqual(f.runtimeExtraArgs, ['--vae-tile-overlap', '0.25']);
    assert.strictEqual(f.files.length, 4, 'все 4 файла (две lora)');
    assert.strictEqual(f.files[0].localPath, '/models/flux/q4.gguf', 'localPath показываем оператору');
    assert.strictEqual(f.files[0].sizeBytes, 6800000000);
    assert.strictEqual(f.files[1].revision, 'main', 'пустая ревизия -> main');
});

check('profileToForm: новый профиль (seedDefaults) получает дефолты семейства', function () {
    const f = P.profileToForm({ family: 'flux' }, true);
    assert.strictEqual(f.defaultsSteps, 4, 'flux: 4 шага (DefaultImageGenDefaults)');
    assert.strictEqual(f.defaultsCfgScale, 1, 'flux: cfg 1.0 (distilled)');
    assert.strictEqual(f.defaultsSampler, 'euler');
    assert.strictEqual(f.defaultsWidth, 1024);
    assert.strictEqual(f.defaultsSeed, -1, 'новый профиль: seed random');
    assert.strictEqual(f.runtimeSeedMode, 'random');
    assert.deepStrictEqual(f.files, [], 'файлов нет - их добавляет оператор');
});

check('profileToForm: без seedDefaults пустые поля остаются пустыми (не подменяем значения)', function () {
    const f = P.profileToForm({ family: 'sd15' }, false);
    assert.strictEqual(f.defaultsSteps, '');
    assert.strictEqual(f.vramEstimateMb, '');
    assert.strictEqual(f.files.length, 0);
});

// --- 2. formToProfile ------------------------------------------------------
check('formToProfile: полная форма -> тело PUT с вложенными defaults/runtime', function () {
    const form = P.profileToForm(FULL_PROFILE, true);
    const body = P.formToProfile(form);
    assert.strictEqual(body.name, 'flux-schnell-q4');
    assert.strictEqual(body.family, 'flux');
    assert.strictEqual(body.defaults.steps, 4);
    assert.strictEqual(body.defaults.cfgScale, 1);
    assert.strictEqual(body.defaults.sampler, 'euler');
    assert.strictEqual(body.defaults.scheduler, 'simple');
    assert.strictEqual(body.defaults.width, 1024);
    assert.strictEqual(body.defaults.height, 768);
    assert.strictEqual(body.defaults.batchCount, 2);
    assert.strictEqual(body.defaults.negativePrompt, 'blurry');
    assert.strictEqual(body.defaults.seed, 42);
    assert.strictEqual(body.defaults.clipSkip, 2);
    assert.strictEqual(body.runtime.backend, 'te=cpu');
    assert.strictEqual(body.runtime.paramsBackend, 'disk');
    assert.strictEqual(body.runtime.offloadToCpu, false, 'булевы уходят ВСЕГДА (presence на сервере)');
    assert.strictEqual(body.runtime.diffusionFa, true);
    assert.strictEqual(body.runtime.vaeTiling, true);
    assert.strictEqual(body.runtime.vaeTileSize, 512);
    assert.strictEqual(body.runtime.vaeConvDirect, true);
    assert.strictEqual(body.runtime.taesd, true);
    assert.strictEqual(body.runtime.splitMode, 'row');
    assert.strictEqual(body.runtime.nGpuLayers, 24);
    assert.strictEqual(body.runtime.threads, 6);
    assert.strictEqual(body.runtime.seedMode, 'fixed');
    assert.deepStrictEqual(body.runtime.extraArgs, ['--vae-tile-overlap', '0.25']);
    assert.strictEqual(body.disabled, true);
    assert.strictEqual(body.notes, 'test note');
    assert.strictEqual(body.vramEstimateMb, 7000);
    assert.strictEqual(body.timeoutSec, 600);
    assert.strictEqual(body.idleUnloadMinutes, 15);
    assert.strictEqual(body.files.length, 4);
    assert.deepStrictEqual(Object.keys(body.files[0]).sort(), ['filename', 'repo', 'revision', 'role'],
        'localPath/sizeBytes не уходят в PUT (сервер их знает сам)');
});

check('formToProfile: пустые строки формы -> дефолты семейства, пустые строки файлов отброшены', function () {
    const body = P.formToProfile({
        name: 'new-one', family: 'z_image',
        defaultsSteps: '', defaultsCfgScale: '', defaultsWidth: '', defaultsHeight: '',
        defaultsBatchCount: '', defaultsSeed: '', defaultsSampler: '', defaultsScheduler: '',
        runtimeExtraArgs: ['', '  ', '--foo'],
        files: [
            { role: 'diffusion', repo: 'leejet/Z-Image-Turbo-GGUF', filename: 'z.gguf', revision: '' },
            { role: '', repo: '', filename: '' },
            { role: 'vae', repo: 'r', filename: 'v.safetensors', revision: 'main' }
        ]
    });
    assert.strictEqual(body.defaults.steps, 8, 'z_image: 8 шагов');
    assert.strictEqual(body.defaults.sampler, 'euler');
    assert.strictEqual(body.defaults.scheduler, 'smoothstep');
    assert.strictEqual(body.defaults.width, 512);
    assert.strictEqual(body.defaults.height, 1024);
    assert.strictEqual(body.defaults.seed, -1, 'пустой seed = random');
    assert.strictEqual(body.defaults.batchCount, 1);
    assert.strictEqual(body.files.length, 2, 'пустая строка файла отброшена');
    assert.strictEqual(body.files[0].revision, 'main');
    assert.deepStrictEqual(body.runtime.extraArgs, ['--foo'], 'пустые строки argv отброшены');
    assert.strictEqual(body.vramEstimateMb, undefined, '0/пусто -> поле не отправляем (мерж не даст обнулить)');
});

check('formToProfile: строка extraArgs (textarea) режется по строкам', function () {
    const body = P.formToProfile({
        name: 'x', family: 'sd15', defaultsSteps: 20, defaultsCfgScale: 7,
        defaultsWidth: 512, defaultsHeight: 512, defaultsBatchCount: 1, defaultsSeed: -1,
        runtimeExtraArgs: '--vae-tile-overlap 0.25\n\n  --foo  \r\n--bar',
        files: [{ role: 'diffusion', repo: 'r', filename: 'f.gguf' }]
    });
    assert.deepStrictEqual(body.runtime.extraArgs, ['--vae-tile-overlap 0.25', '--foo', '--bar']);
});

check('splitArgsText: null/undefined/пусто -> []', function () {
    assert.deepStrictEqual(P.splitArgsText(null), []);
    assert.deepStrictEqual(P.splitArgsText(undefined), []);
    assert.deepStrictEqual(P.splitArgsText('  \n \t '), []);
});

// --- 3. validateForm -------------------------------------------------------
function validForm(overrides) {
    const base = {
        name: 'sd15-q8', family: 'sd15',
        files: [{ role: 'diffusion', repo: 'r', filename: 'f.gguf', revision: 'main' }],
        defaultsSteps: 25, defaultsCfgScale: 7, defaultsSampler: 'euler_a', defaultsScheduler: 'discrete',
        defaultsWidth: 512, defaultsHeight: 512, defaultsBatchCount: 1, defaultsSeed: -1, defaultsClipSkip: 0,
        runtimeNGpuLayers: 0, runtimeAutoFit: '', runtimeSplitMode: '', runtimeVaeTileSize: 0,
        runtimeSeedMode: 'random', runtimeThreads: 0, runtimeOffloadToCpu: false, runtimeParamsBackend: '',
        vramEstimateMb: 0, timeoutSec: 0, idleUnloadMinutes: 0
    };
    return Object.assign(base, overrides || {});
}

check('validateForm: корректная форма - ошибок нет', function () {
    const res = P.validateForm(validForm());
    assert.deepStrictEqual(res.errors, []);
    assert.strictEqual(res.ok, true);
});

check('validateForm: пустое имя и отсутствие diffusion', function () {
    const res = P.validateForm(validForm({ name: '  ', files: [] }));
    const codes = res.errors.map(function (e) { return e.code; });
    assert.ok(codes.indexOf('name_required') !== -1, 'нужно name_required');
    assert.ok(codes.indexOf('need_diffusion') !== -1, 'нужно need_diffusion');
    assert.strictEqual(res.ok, false);
});

check('validateForm: неизвестная роль и неизвестное семейство', function () {
    const res = P.validateForm(validForm({
        family: 'nope',
        files: [{ role: 'unet', repo: 'r', filename: 'f' }]
    }));
    const codes = res.errors.map(function (e) { return e.code; });
    assert.ok(codes.indexOf('bad_family') !== -1);
    assert.ok(codes.indexOf('file_role_unknown') !== -1);
});

check('validateForm: строка без repo/filename - ошибка с номером строки', function () {
    const res = P.validateForm(validForm({
        files: [
            { role: 'diffusion', repo: 'r', filename: 'f.gguf' },
            { role: 'vae', repo: '', filename: 'v.safetensors' }
        ]
    }));
    const err = res.errors.filter(function (e) { return e.code === 'file_missing_fields'; })[0];
    assert.ok(err, 'нужна ошибка file_missing_fields');
    assert.strictEqual(err.row, 2);
    assert.strictEqual(err.role, 'vae');
});

check('validateForm: дубль роли запрещён, lora - исключение', function () {
    const dup = P.validateForm(validForm({
        files: [
            { role: 'diffusion', repo: 'r', filename: 'f.gguf' },
            { role: 'vae', repo: 'r', filename: 'v1.safetensors' },
            { role: 'vae', repo: 'r', filename: 'v2.safetensors' }
        ]
    }));
    const err = dup.errors.filter(function (e) { return e.code === 'file_dup_role'; })[0];
    assert.ok(err, 'дубль vae должен быть ошибкой');
    assert.strictEqual(err.role, 'vae');
    assert.strictEqual(err.row, 3);

    const loras = P.validateForm(validForm({
        files: [
            { role: 'diffusion', repo: 'r', filename: 'f.gguf' },
            { role: 'lora', repo: 'r', filename: 'a.safetensors' },
            { role: 'lora', repo: 'r', filename: 'b.safetensors' }
        ]
    }));
    assert.deepStrictEqual(loras.errors, [], 'две lora - это нормально');
});

check('validateForm: размеры - диапазон и кратность 64', function () {
    const res = P.validateForm(validForm({ defaultsWidth: 513, defaultsHeight: 32 }));
    const codes = res.errors.map(function (e) { return e.code; });
    assert.ok(codes.indexOf('bad_side_multiple') !== -1, '513 не кратно 64');
    assert.ok(codes.indexOf('bad_side') !== -1, '32 < 64');
    const multi = res.errors.filter(function (e) { return e.code === 'bad_side_multiple'; })[0];
    assert.strictEqual(multi.side, 'width');
    assert.strictEqual(multi.step, 64);
    assert.strictEqual(multi.value, 513);
    const range = res.errors.filter(function (e) { return e.code === 'bad_side'; })[0];
    assert.strictEqual(range.side, 'height');
    // 4096 кратно 64 и в границах - ок
    assert.deepStrictEqual(P.validateForm(validForm({ defaultsWidth: 4096, defaultsHeight: 64 })).errors, []);
});

check('validateForm: steps/cfg/batch/seed вне диапазонов', function () {
    const res = P.validateForm(validForm({ defaultsSteps: 0, defaultsCfgScale: 31, defaultsBatchCount: 9, defaultsSeed: -2 }));
    const codes = res.errors.map(function (e) { return e.code; });
    assert.ok(codes.indexOf('bad_steps') !== -1);
    assert.ok(codes.indexOf('bad_cfg') !== -1);
    assert.ok(codes.indexOf('bad_batch') !== -1);
    assert.ok(codes.indexOf('bad_seed') !== -1);
});

check('validateForm: offloadToCpu + paramsBackend вместе - ошибка', function () {
    const res = P.validateForm(validForm({ runtimeOffloadToCpu: true, runtimeParamsBackend: 'cpu' }));
    const codes = res.errors.map(function (e) { return e.code; });
    assert.ok(codes.indexOf('offload_params_conflict') !== -1);
    // только offload - ок; только paramsBackend - ок
    assert.deepStrictEqual(P.validateForm(validForm({ runtimeOffloadToCpu: true })).errors, []);
    assert.deepStrictEqual(P.validateForm(validForm({ runtimeParamsBackend: 'disk' })).errors, []);
});

check('validateForm: DiT-семейства требуют отдельный vae', function () {
    const ditFamilies = ['sd3', 'flux', 'flux2', 'chroma', 'qwen_image', 'z_image'];
    ditFamilies.forEach(function (fam) {
        const res = P.validateForm(validForm({ family: fam }));
        const codes = res.errors.map(function (e) { return e.code; });
        assert.ok(codes.indexOf('dit_need_vae') !== -1, fam + ' должен требовать vae');
        assert.ok(codes.indexOf('need_diffusion') === -1, fam + ': diffusion есть');
    });
    // С vae - валидно
    assert.deepStrictEqual(P.validateForm(validForm({
        family: 'flux',
        files: [
            { role: 'diffusion', repo: 'r', filename: 'f.gguf' },
            { role: 'vae', repo: 'r', filename: 'ae.safetensors' }
        ]
    })).errors, []);
    // sd15 (all-in-one) vae не требует
    assert.deepStrictEqual(P.validateForm(validForm({ family: 'sd15' })).errors, []);
});

check('validateForm: runtime-перечисления и неотрицательные числа', function () {
    const res = P.validateForm(validForm({
        runtimeAutoFit: 'yes', runtimeSplitMode: 'diagonal', runtimeSeedMode: 'sometimes',
        runtimeVaeTileSize: -8, runtimeThreads: -2, runtimeNGpuLayers: 1000,
        vramEstimateMb: -1, timeoutSec: -1, idleUnloadMinutes: -1
    }));
    const codes = res.errors.map(function (e) { return e.code; });
    ['bad_auto_fit', 'bad_split_mode', 'bad_seed_mode', 'bad_vae_tile_size', 'bad_threads',
        'bad_gpu_layers', 'bad_vram_estimate', 'bad_timeout', 'bad_idle_unload'].forEach(function (c) {
        assert.ok(codes.indexOf(c) !== -1, 'нужна ошибка ' + c);
    });
    assert.deepStrictEqual(P.validateForm(validForm({ runtimeNGpuLayers: -1 })).errors, [],
        'nGpuLayers -1 (все слои на GPU) валиден');
});

check('validateForm: пустые строки файлов игнорируются', function () {
    const res = P.validateForm(validForm({
        files: [
            { role: 'diffusion', repo: 'r', filename: 'f.gguf' },
            { role: '', repo: '', filename: '' }
        ]
    }));
    assert.deepStrictEqual(res.errors, []);
});

// --- 4. normalizeApplyResponse --------------------------------------------
check('normalizeApplyResponse: persisted -> applied', function () {
    const snap = P.normalizeApplyResponse({
        model: 'sd15-q8', applyId: 'imgapply-1', status: 'completed',
        backends: [
            { backendId: 'img-1', status: 'persisted', message: 'saved' },
            { backendId: 'img-2', status: 'persisted' }
        ]
    });
    assert.strictEqual(snap.model, 'sd15-q8');
    assert.strictEqual(snap.applyId, 'imgapply-1');
    assert.strictEqual(snap.total, 2);
    assert.strictEqual(snap.persisted, 2);
    assert.strictEqual(snap.outcome, 'applied');
    assert.strictEqual(snap.done, true);
    assert.strictEqual(snap.running, false);
});

check('normalizeApplyResponse: unreachable не превращается в успех', function () {
    const snap = P.normalizeApplyResponse({
        model: 'sd15-q8', applyId: 'a2', status: 'completed',
        backends: [{ backendId: 'img-1', status: 'unreachable', message: 'connection refused' }]
    });
    assert.strictEqual(snap.unreachable, 1);
    assert.strictEqual(snap.persisted, 0);
    assert.strictEqual(snap.outcome, 'unreachable');
    assert.strictEqual(snap.backends[0].message, 'connection refused');
});

check('normalizeApplyResponse: часть упала -> partial, running -> не done', function () {
    const partial = P.normalizeApplyResponse({
        model: 'm', status: 'completed',
        backends: [
            { backendId: 'a', status: 'persisted' },
            { backendId: 'b', status: 'error', message: 'HTTP 500' }
        ]
    });
    assert.strictEqual(partial.outcome, 'partial');
    assert.strictEqual(partial.failed, 1);

    const running = P.normalizeApplyResponse({ model: 'm', applyId: 'x', status: 'running', backends: [] });
    assert.strictEqual(running.running, true);
    assert.strictEqual(running.done, false);
    assert.strictEqual(running.outcome, 'running');

    const noBackends = P.normalizeApplyResponse({ model: 'm', status: 'completed', backends: [] });
    assert.strictEqual(noBackends.outcome, 'unknown');
    assert.strictEqual(noBackends.done, true);
});

check('normalizeApplyResponse: обёртки {progress:...}, {data:...} и голый список', function () {
    const wrapped = P.normalizeApplyResponse({ progress: { model: 'm', status: 'completed', backends: [{ backendId: 'a', status: 'persisted' }] } });
    assert.strictEqual(wrapped.outcome, 'applied');
    const dataWrapped = P.normalizeApplyResponse({ data: { model: 'm', status: 'completed', backends: [{ backendId: 'a', status: 'persisted' }] } });
    assert.strictEqual(dataWrapped.persisted, 1);
    const barelist = P.normalizeApplyResponse([{ backendId: 'a', status: 'persisted' }]);
    assert.strictEqual(barelist.total, 1);
    assert.strictEqual(barelist.model, '');
});

// --- 5. списки / бэкенды / ошибки -----------------------------------------
check('normalizeProfileList: {models:{...}} -> сортированный массив', function () {
    const list = P.normalizeProfileList({
        models: {
            'z-model': { family: 'z_image' },
            'a-model': { name: 'a-model', family: 'sd15' }
        }, total: 2
    });
    assert.deepStrictEqual(list.map(function (e) { return e.name; }), ['a-model', 'z-model']);
    assert.strictEqual(list[1].profile.name, 'z-model', 'имя из ключа подставляется в профиль');
    assert.deepStrictEqual(P.normalizeProfileList(null), []);
    assert.deepStrictEqual(P.normalizeProfileList([]), []);
});

check('normalizeCatalog: {presets:[...]} и голый массив', function () {
    const c1 = P.normalizeCatalog({ presets: [{ name: 'sd15-q8', family: 'sd15' }], total: 1 });
    assert.strictEqual(c1.length, 1);
    assert.strictEqual(c1[0].name, 'sd15-q8');
    const c2 = P.normalizeCatalog([{ family: 'flux' }]);
    assert.strictEqual(c2[0].name, 'flux', 'без имени берём семейство');
    assert.deepStrictEqual(P.normalizeCatalog(null), []);
});

check('normalizeBackends: фильтр image-типов для общего списка', function () {
    const all = [
        { id: 'llama-1', type: 'llama_cpp', host: 'h1' },
        { id: 'img-1', name: 'GPU image', type: 'image_cpp', host: 'h2', imagePort: 18093, status: 'healthy' },
        { id: 'img-2', type: 'sd_cpp', host: 'h3' }
    ];
    const imageOnly = P.normalizeBackends(all, true);
    assert.deepStrictEqual(imageOnly.map(function (b) { return b.id; }), ['img-1', 'img-2']);
    assert.strictEqual(imageOnly[0].imagePort, 18093);
    assert.strictEqual(P.normalizeBackends(all, false).length, 3);
    assert.strictEqual(P.normalizeBackends({ backends: all }, true).length, 2);
    assert.deepStrictEqual(P.normalizeBackends(null, true), []);
});

check('parseApiError: плоский конверт, {error:{message}}, сырой текст, пустой ответ', function () {
    assert.strictEqual(P.parseApiError({ error: 'invalid profile', message: 'steps must be in [1,100]' }, 400),
        'invalid profile: steps must be in [1,100]');
    assert.strictEqual(P.parseApiError({ error: { message: 'no healthy backend' } }, 503), 'no healthy backend');
    assert.strictEqual(P.parseApiError('boom', 500), 'boom');
    assert.strictEqual(P.parseApiError('{"error":"profile not found"}', 404), 'profile not found');
    assert.strictEqual(P.parseApiError('', 502), 'HTTP 502');
    assert.strictEqual(P.parseApiError(null, 0), 'HTTP ?');
});

check('parseApiError: длинный HTML-ответ обрезается', function () {
    const long = 'x'.repeat(500);
    const out = P.parseApiError(long, 500);
    assert.strictEqual(out.length, 403, '400 символов + "..."');
    assert.ok(out.slice(-3) === '...');
});

// --- 6. хелперы ------------------------------------------------------------
check('familyDefaultsFor: ключевые семейства (см. DefaultImageGenDefaults)', function () {
    assert.strictEqual(P.familyDefaultsFor('sd15').steps, 25);
    assert.strictEqual(P.familyDefaultsFor('sdxl').width, 1024);
    assert.strictEqual(P.familyDefaultsFor('sdxl_turbo').steps, 4);
    assert.strictEqual(P.familyDefaultsFor('sdxl_turbo').cfgScale, 1);
    assert.strictEqual(P.familyDefaultsFor('sd3').steps, 28);
    assert.strictEqual(P.familyDefaultsFor('flux').steps, 4);
    assert.strictEqual(P.familyDefaultsFor('chroma').cfgScale, 4);
    assert.strictEqual(P.familyDefaultsFor('qwen_image').cfgScale, 2.5);
    assert.strictEqual(P.familyDefaultsFor('z_image').scheduler, 'smoothstep');
    assert.strictEqual(P.familyDefaultsFor('z_image').height, 1024);
    assert.strictEqual(P.familyDefaultsFor('other').steps, 20);
    assert.strictEqual(P.familyDefaultsFor(undefined).seed, -1);
});

check('isDitFamily / isValidFamily / isValidRole', function () {
    assert.strictEqual(P.isDitFamily('flux'), true);
    assert.strictEqual(P.isDitFamily('sd15'), false);
    assert.strictEqual(P.isDitFamily(''), false);
    assert.strictEqual(P.isValidFamily('z_image'), true);
    assert.strictEqual(P.isValidFamily('sdxl_turbo'), true);
    assert.strictEqual(P.isValidFamily('nope'), false);
    assert.strictEqual(P.isValidRole('ip_adapter'), true);
    assert.strictEqual(P.isValidRole('unet'), false);
    assert.strictEqual(P.IMAGE_ROLES.length, 12, '12 ролей - контракт pkg/types/image_model.go');
    assert.strictEqual(P.IMAGE_FAMILIES.length, 12, '12 семейств');
});

check('bundleSizeBytes / formatBytes / formatDuration', function () {
    assert.strictEqual(P.bundleSizeBytes([
        { sizeBytes: 1000 }, { sizeBytes: 2000 }, { size_bytes: 500 }, { sizeBytes: 0 }, null
    ]), 3500);
    assert.strictEqual(P.bundleSizeBytes(null), 0);
    assert.strictEqual(P.formatBytes(3500), '3.4 KB');
    assert.strictEqual(P.formatBytes(1536), '1.5 KB');
    assert.strictEqual(P.formatBytes(5368709120), '5.0 GB');
    assert.strictEqual(P.formatBytes(0), '0 B');
    assert.strictEqual(P.formatBytes('nope'), '-');
    assert.strictEqual(P.formatBytes(null), '-');
    assert.strictEqual(P.formatDuration(0), '0s');
    assert.strictEqual(P.formatDuration(45000), '45s');
    assert.strictEqual(P.formatDuration(65000), '1m 05s');
    assert.strictEqual(P.formatDuration(3720000), '1h 02m');
});

check('escapeHtml: экранирует кавычки и угловые скобки (защита от инъекции имён)', function () {
    assert.strictEqual(P.escapeHtml('<img src=x onerror="a">'), '&lt;img src=x onerror=&quot;a&quot;&gt;');
    assert.strictEqual(P.escapeHtml("it's"), 'it&#39;s');
    assert.strictEqual(P.escapeHtml(null), '');
});

check('clampInt / intOrNull / floatOrNull', function () {
    assert.strictEqual(P.clampInt('150', 1, 100, 20), 100);
    assert.strictEqual(P.clampInt('abc', 1, 100, 20), 20);
    assert.strictEqual(P.clampInt('', 0, 10, 5), 5);
    assert.strictEqual(P.intOrNull(' 42 '), 42);
    assert.strictEqual(P.intOrNull('-1'), -1);
    assert.strictEqual(P.intOrNull('4.5'), null, 'не целое - не число формы');
    assert.strictEqual(P.intOrNull(''), null);
    assert.strictEqual(P.floatOrNull('1.5'), 1.5);
    assert.strictEqual(P.floatOrNull('x'), null);
    assert.strictEqual(P.floatOrNull(undefined), null);
});

// --- 7. DOM-хелперы модуля (fake-документ, без браузера) -------------------
check('formStateFromDom: имена data-imgp-field маппятся в контракт формы', function () {
    const doc = IP._actions.formStateFromDom(makeFakeEditorDoc({
        imgpName: 'my-profile', imgpFamily: 'flux', imgpNotes: 'n',
        imgpVram: '7000', imgpTimeout: '600', imgpIdleUnload: '15',
        imgpSteps: '4', imgpCfg: '1', imgpWidth: '1024', imgpHeight: '768', imgpBatch: '1',
        imgpSeed: '-1', imgpClipSkip: '0', imgpSampler: 'euler', imgpScheduler: 'simple',
        imgpNegative: 'ugly', imgpRuntimeBackend: 'te=cpu', imgpParamsBackend: '',
        imgpMaxVram: '6', imgpAutoFit: 'on', imgpSplitMode: 'row', imgpGpuLayers: '24',
        imgpThreads: '6', imgpVaeTileSize: '512', imgpSeedMode: 'random',
        imgpExtraArgs: '--foo\n--bar'
    }, { imgpDisabled: true, imgpOffloadToCpu: false, imgpDiffusionFa: true }, [
        { role: 'diffusion', repo: 'leejet/FLUX.1-schnell-gguf', filename: 'q4.gguf', revision: 'main' },
        { role: 'vae', repo: 'black-forest-labs/FLUX.1-schnell', filename: 'ae.safetensors', revision: 'main' }
    ]));
    assert.strictEqual(doc.name, 'my-profile');
    assert.strictEqual(doc.family, 'flux');
    assert.strictEqual(doc.disabled, true);
    assert.strictEqual(doc.defaultsSteps, '4');
    assert.strictEqual(doc.runtimeBackend, 'te=cpu');
    assert.strictEqual(doc.runtimeDiffusionFa, true);
    assert.strictEqual(doc.runtimeOffloadToCpu, false);
    assert.strictEqual(doc.runtimeSeedMode, 'random');
    assert.deepStrictEqual(doc.runtimeExtraArgs, ['--foo', '--bar']);
    assert.strictEqual(doc.files.length, 2);
    // Ключевой момент: то, что прочитано из DOM, проходит валидацию без правок.
    assert.deepStrictEqual(P.validateForm(doc).errors, []);
    // ...и превращается в корректное тело PUT.
    const body = P.formToProfile(doc);
    assert.strictEqual(body.name, 'my-profile');
    assert.strictEqual(body.runtime.vaeTileSize, 512);
    assert.strictEqual(body.runtime.autoFit, 'on');
    assert.strictEqual(body.files.length, 2);
});

check('formStateFromDom: пустая форма -> ошибки имени/diffusion/диапазонов', function () {
    const doc = IP._actions.formStateFromDom(makeFakeEditorDoc({}, {}, []));
    const codes = P.validateForm(doc).errors.map(function (e) { return e.code; });
    assert.ok(codes.indexOf('name_required') !== -1);
    assert.ok(codes.indexOf('need_diffusion') !== -1);
    assert.ok(codes.indexOf('bad_steps') !== -1, 'пустые числа - ошибка, а не молчаливый дефолт');
});

check('previewServerArgs: флаги собираются по ролям и runtime', function () {
    const args = IP._actions.previewServerArgs(P.formToProfile(P.profileToForm(FULL_PROFILE, true)));
    assert.ok(args.indexOf('--diffusion-model') !== -1, 'flux - DiT: --diffusion-model, не --model');
    assert.ok(args.indexOf('--vae') !== -1);
    assert.ok(args.indexOf('--vae-tiling') !== -1);
    assert.ok(args.indexOf('--vae-tile-size') !== -1);
    assert.ok(args.indexOf('--taesd') !== -1);
    assert.ok(args.indexOf('--backend') !== -1);
    assert.ok(args.indexOf('--params-backend') !== -1, 'paramsBackend имеет приоритет над offload');
    assert.ok(args.indexOf('--offload-to-cpu') === -1, 'offload не дублируется при paramsBackend');
    assert.ok(args.indexOf('--seed') === -1, 'seedMode=fixed -> --seed -1 не добавляется');
    assert.deepStrictEqual(args.slice(-2), ['--vae-tile-overlap', '0.25'], 'extraArgs идут последними');

    const sd15 = IP._actions.previewServerArgs(P.formToProfile(P.profileToForm({
        name: 'a', family: 'sd15',
        files: [{ role: 'diffusion', repo: 'r', filename: 'f.gguf' }],
        defaults: { steps: 25, cfgScale: 7, width: 512, height: 512, batchCount: 1, seed: -1 },
        runtime: { seedMode: 'random' }
    }, true)));
    assert.ok(sd15.indexOf('--model') !== -1, 'all-in-one грузится через --model');
    assert.ok(sd15.indexOf('--diffusion-model') === -1);
    assert.ok(sd15.indexOf('--seed') !== -1, 'random seed -> --seed -1 явно');
});

// --- fake DOM для DOM-хелперов -------------------------------------------
/**
 * Мини-документ редактора: набор input'ов с data-imgp-field + строки файлов.
 * Тест проверяет маппинг имён и валидацию, а не реальный разбор HTML
 * (реальную разметку к DOM проверяет image-profiles-dom.test.js).
 */
function makeFakeEditorDoc(fields, checks, rows) {
    const nodes = [];
    Object.keys(fields || {}).forEach(function (id) {
        nodes.push({ attr: id, value: String(fields[id]), type: 'text', getAttribute: function (n) { return n === 'data-imgp-field' ? id : null; } });
    });
    Object.keys(checks || {}).forEach(function (id) {
        nodes.push({ attr: id, value: '', type: 'checkbox', checked: !!checks[id], getAttribute: function (n) { return n === 'data-imgp-field' ? id : null; } });
    });
    const fileRows = (rows || []).map(function (r, i) {
        return {
            attr: String(i),
            querySelector: function (sel) {
                const m = /^\[data-imgp-file="([a-z]+)"\]$/.exec(sel);
                if (!m) return null;
                const field = m[1];
                return { value: r[field] === undefined ? '' : String(r[field]) };
            }
        };
    });
    return {
        querySelectorAll: function (sel) {
            if (sel === '[data-imgp-file-row]') return fileRows;
            if (sel === '[data-imgp-field]') return nodes;
            return [];
        },
        querySelector: function (sel) {
            const m = /^\[data-imgp-field="([^"]+)"\]$/.exec(sel);
            if (!m) return null;
            return nodes.filter(function (n) { return n.attr === m[1]; })[0] || null;
        }
    };
}

console.log('\n' + (failures.length ? 'FAILED: ' + failures.length : 'OK: ' + passed + ' checks passed'));
if (failures.length) {
    failures.forEach(function (f) { console.error(' - ' + f); });
    process.exit(1);
}
