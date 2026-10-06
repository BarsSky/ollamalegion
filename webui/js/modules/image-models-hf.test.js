// image-models-hf.test.js — R-Image Phase 9: табы «HuggingFace» и «Загрузки»
// страницы «Image-модели» (модуль image-models-hf.js).
//
// Запуск: node webui/js/modules/image-models-hf.test.js
//
// ЧТО ПРОВЕРЯЕМ (и почему именно это):
//   1. Поиск: GET /api/v1/image/backends/{id}/hf/search с query+limit+task и
//      заголовком X-HF-Token (без токена приватные репозитории не видны, а
//      воркер читает его ТОЛЬКО из заголовка — cmd/sdworker/handlers_hf.go:107).
//   2. Файлы репозитория: GET .../hf/files?modelId=... — роли берутся из
//      СЕРВЕРНОГО suggestedRole; в JS нет своей таблицы «имя файла → роль»
//      (иначе правила разъехались бы с internal/sdbackend.SuggestRole).
//   3. Отметки по умолчанию: diffusion/vae/clip_l/clip_g — да, t5xxl — нет
//      (крупный файл: скачивать его «за компанию» дорого).
//   4. Сборка bundle: тело POST .../hf/bundle = {name, family, files:[{role,
//      repo, filename, revision, sizeBytes}]}, дубликат роли — ошибка (кроме lora),
//      без diffusion — ошибка, пустой выбор — ошибка.
//   5. Прогресс: опрос .../hf/progress?bundleId= (агрегат воркера) и остановка
//      поллинга по финальному статусу; повторное чтение списка моделей, чтобы
//      новая модель появилась на табе «Модели на диске».
//   6. «Загрузки»: снимок /hf/downloads раскладывается на активные bundle,
//      активные файлы, историю и остатки; очистка остатка идёт DELETE
//      /hf/cleanup?filename=... (имя вида «bundle/file» в путь не положить).
//   7. НИЧЕГО НЕ ПОКАЗЫВАЕТ КАРТИНКИ: в модуле нет ни b64, ни <img> из ответа
//      генерации (Phase 9 убрала показ сгенерированного из WebUI).
//   8. ПОМЕТКИ ПО ЗАГОЛОВКУ (Phase 10): пред-проверка файла
//      GET .../hf/probe?modelId=&filename=&revision= (без скачивания весов) и
//      автоподстановка семейства профиля по вердикту движка. Приговоров
//      «движок это не прочитает» нет: «голые» diffusers-имена тензоров есть и у
//      официальных сборок под sd.cpp, а DiT-файл, подключённый как all-in-one,
//      движок не узнаёт («get sd version from file failed») — именно это
//      расхождение ловит diTFamilyWarning, а семейство подставляет
//      applyProbedFamily (ручной выбор оператора не перебивается).
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

// --- mocks: DOM -------------------------------------------------------------

function makeElement(id) {
    const classes = new Set();
    const listeners = {};
    const attrs = {};
    const el = {
        id: id,
        style: {},
        value: '',
        checked: false,
        _html: '',
        textContent: '',
        getAttribute: function (n) { return Object.prototype.hasOwnProperty.call(attrs, n) ? attrs[n] : null; },
        setAttribute: function (n, v) { attrs[n] = v; },
        addEventListener: function (type, fn) { (listeners[type] = listeners[type] || []).push(fn); },
        removeEventListener: function () { },
        dispatch: function (type, ev) { (listeners[type] || []).forEach(function (fn) { fn(ev || {}); }); },
        classList: {
            add: function (c) { classes.add(c); },
            remove: function (c) { classes.delete(c); },
            contains: function (c) { return classes.has(c); },
            toggle: function (c, on) { if (on === undefined) { classes.has(c) ? classes.delete(c) : classes.add(c); } else if (on) { classes.add(c); } else { classes.delete(c); } },
        },
        appendChild: function () { },
        parentNode: null,
    };
    Object.defineProperty(el, 'innerHTML', {
        get: function () { return this._html; },
        set: function (v) { this._html = String(v); },
    });
    return el;
}

const elements = {};
function getEl(id) {
    if (!Object.prototype.hasOwnProperty.call(elements, id)) elements[id] = makeElement(id);
    return elements[id];
}
global.window = global;
global.document = {
    getElementById: getEl,
    querySelector: function () { return null; },
    querySelectorAll: function () { return []; },
    createElement: function (tag) { return makeElement('created-' + tag); },
    addEventListener: function () { },
};
const store = {};
global.localStorage = {
    getItem: function (k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem: function (k, v) { store[k] = String(v); },
    removeItem: function (k) { delete store[k]; },
};
global.WEBUI_CONFIG = { API_BASE: 'http://balancer.test:18081', API_TOKEN: 'test-token' };
global.Api = { getAuthHeaders: function () { return { 'Content-Type': 'application/json', 'X-API-Token': 'test-token' }; } };

const RU = {
    'gguf.searching_for': 'Поиск "{q}"...',
    'gguf.search_error': 'Ошибка поиска',
    'gguf.search_btn': 'Найти',
    'gguf.no_results': 'Результатов не найдено',
    'gguf.no_results_for': 'По запросу «{q}» ничего не найдено',
    'gguf.results_count': 'Найдено моделей: {n}',
    'gguf.download': 'Скачать',
    'gguf.downloading': 'Загрузка...',
    'gguf.download_completed': 'Загрузка завершена',
    'gguf.download_failed': 'Ошибка загрузки',
    'gguf.download_cancelled': 'Загрузка отменена',
    'gguf.active_downloads': 'Активные загрузки',
    'gguf.download_history': 'История загрузок',
    'gguf.delete': 'Удалить',
    'gguf.delete_from_disk': 'Удалить скачанный файл с диска',
    'gguf.file_deleted': 'Файл удалён',
    'gguf.orphans_hint': 'Частичные .download файлы от прерванных загрузок.',
    'gguf.last_updated': 'Обновлён',
    'gguf.likes': 'Лайки',
    'gguf.downloads': 'Загрузок',
    'common.loading': 'Загрузка...',
    'common.error': 'Ошибка',
    'common.unknown': 'unknown',
    'image.no_backend_selected': 'Сначала выберите image-бэкенд',
    'image.bundle_all_done': 'Все файлы bundle скачаны',
    'image.bundle_started': 'Загрузка bundle начата',
    'image.bundle_need_name': 'Нужно имя bundle',
    'image.bundle_need_diffusion': 'В bundle нужен файл diffusion',
    'image.bundle_dup_role': 'Дубликат роли {role}',
    'imageModels.role_diffusion': 'diffusion (веса)',
    'imageModels.role_vae': 'vae',
    'imageModels.hf_selected_count': 'Выбрано файлов: {n}',
    'imageModels.hf_none_selected': 'Отметьте хотя бы один файл',
    'imageModels.hf_bad_role': 'Неизвестная роль: {role}',
    'imageModels.hf_suggested': 'предложено',
    'imageModels.status_interrupted': 'прервано',
    'imageModels.bundle_registered': 'Bundle зарегистрирован',
    'imageModels.bundle_failed': 'Загрузка bundle не удалась',
    'imageModels.dl_files_active': 'Активные загрузки файлов',
    'imageModels.orphans_title': 'Остаточные файлы',
    'imageModels.no_downloads': 'Загрузок нет',
    'imageModels.no_downloads_hint': 'Запустите загрузку на табе HuggingFace.',
};
global.I18N = {
    t: function (k, vars) {
        var s = Object.prototype.hasOwnProperty.call(RU, k) ? RU[k] : k;
        if (vars) Object.keys(vars).forEach(function (p) { s = String(s).replace('{' + p + '}', vars[p]); });
        return s;
    },
    getLang: function () { return 'ru'; },
};
global.console = console;

// --- mocks: fetch -----------------------------------------------------------

const requests = [];
let responseFor = function () { return { status: 200, body: {} }; };

global.fetch = function (url, init) {
    const rec = {
        url: String(url),
        method: (init && init.method) || 'GET',
        headers: (init && init.headers) || {},
        body: init && init.body ? JSON.parse(init.body) : null,
    };
    requests.push(rec);
    const r = responseFor(rec);
    return Promise.resolve({
        ok: r.status < 400,
        status: r.status,
        text: function () { return Promise.resolve(JSON.stringify(r.body)); },
    });
};

require('./image-models-hf.js');
const Hf = global.ImageModelsHf;
assert.ok(Hf, 'window.ImageModelsHf должен быть экспортирован (см. check_iife_exports.py)');
const P = Hf.pure;

// Кооперация с shell/соседним модулем: refresh списка моделей после успешной
// загрузки и тосты. Объявлено ДО асинхронной части — она стартует сразу.
let refreshCalls = 0;
const toasts = [];
global.Toast = {
    show: function (opts) {
        toasts.push({ msg: (opts && opts.message) || '', type: opts && opts.type });
    },
};
global.ImagePage = {
    _actions: {
        refresh: function () { refreshCalls++; return Promise.resolve(); },
    },
};

// --- helpers ----------------------------------------------------------------

let passed = 0;
const failures = [];
function check(name, fn) {
    try {
        fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' — ' + (e && e.message));
    }
}
const sleep = function (ms) { return new Promise(function (r) { setTimeout(r, ms); }); };

const ctx = {
    apiBase: 'http://balancer.test:18081',
    headers: { 'Content-Type': 'application/json', 'X-API-Token': 'test-token' },
    backendId: 'img-1',
    backend: { id: 'img-1', type: 'image_cpp' },
    backends: [{ id: 'img-1', type: 'image_cpp' }],
    tab: 'hf',
};

const FILES = [
    { path: 'flux1-schnell-Q4_0.gguf', sizeBytes: 4000000000, suggestedRole: 'diffusion' },
    { path: 'ae.safetensors', sizeBytes: 335000000, suggestedRole: 'vae' },
    { path: 't5xxl-Q4_K_M.gguf', sizeBytes: 5000000000, suggestedRole: 't5xxl' },
    { path: 'clip_l.safetensors', sizeBytes: 246000000, suggestedRole: 'clip_l' },
];

// ============================================================================
// 1. Чистые хелперы
// ============================================================================

check('роли: 12 значений из pkg/types/image_model.go', function () {
    assert.strictEqual(P.IMAGE_ROLES.length, 12);
    ['diffusion', 'vae', 'clip_l', 'clip_g', 't5xxl', 'llm', 'clip_vision', 'taesd', 'lora', 'upscaler', 'controlnet', 'ip_adapter']
        .forEach(function (r) { assert.ok(P.IMAGE_ROLES.indexOf(r) >= 0, 'роль ' + r); });
});

check('семейства: 12 значений из pkg/types/image_model.go', function () {
    assert.strictEqual(P.IMAGE_FAMILIES.length, 12);
    ['sd15', 'sd21', 'sd_turbo', 'sdxl', 'sdxl_turbo', 'sd3', 'flux', 'flux2', 'chroma', 'qwen_image', 'z_image', 'other']
        .forEach(function (f) { assert.ok(P.IMAGE_FAMILIES.indexOf(f) >= 0, 'семейство ' + f); });
});

check('defaultSelection: diffusion/vae/clip_l/clip_g отмечены, t5xxl — нет', function () {
    const sel = P.defaultSelection(FILES, null);
    assert.strictEqual(sel['flux1-schnell-Q4_0.gguf'].checked, true, 'diffusion должна быть отмечена');
    assert.strictEqual(sel['ae.safetensors'].checked, true, 'vae должна быть отмечена');
    assert.strictEqual(sel['clip_l.safetensors'].checked, true, 'clip_l должна быть отмечена');
    assert.strictEqual(sel['t5xxl-Q4_K_M.gguf'].checked, false, 't5xxl — крупный файл, только вручную');
    assert.strictEqual(sel['t5xxl-Q4_K_M.gguf'].role, 't5xxl', 'роль всё равно подставлена сервером');
});

check('defaultSelection: одна роль — один отмеченный файл (первый по порядку)', function () {
    const files = [
        { path: 'model-f16.gguf', suggestedRole: 'diffusion' },
        { path: 'model-q4.gguf', suggestedRole: 'diffusion' },
    ];
    const sel = P.defaultSelection(files, null);
    assert.strictEqual(sel['model-f16.gguf'].checked, true);
    assert.strictEqual(sel['model-q4.gguf'].checked, false);
});

check('defaultSelection: выбор пользователя (preserve) не сбрасывается', function () {
    const prev = { 't5xxl-Q4_K_M.gguf': { checked: true, role: 't5xxl' }, 'ae.safetensors': { checked: false, role: 'vae' } };
    const sel = P.defaultSelection(FILES, prev);
    assert.strictEqual(sel['t5xxl-Q4_K_M.gguf'].checked, true, 'галочка t5xxl должна пережить повторный рендер');
    assert.strictEqual(sel['ae.safetensors'].checked, false, 'снятая галочка тоже переживает рендер');
});

check('buildFileSpecs: roles/repo/filename/revision/sizeBytes для POST /hf/bundle', function () {
    const sel = P.defaultSelection(FILES, null);
    const built = P.buildFileSpecs('leejet/FLUX.1-schnell-gguf', sel, { name: 'flux-schnell-q4' });
    assert.deepStrictEqual(built.errors, []);
    assert.strictEqual(built.name, 'flux-schnell-q4');
    assert.strictEqual(built.files.length, 3);
    assert.strictEqual(built.files[0].repo, 'leejet/FLUX.1-schnell-gguf');
    assert.strictEqual(built.files[0].revision, 'main');
    assert.ok(built.files.every(function (f) { return f.sizeBytes > 0; }), 'sizeBytes нужен воркеру для пропуска уже скачанного');
});

check('buildFileSpecs: без имени / без файлов / без diffusion — коды ошибок', function () {
    const sel = P.defaultSelection(FILES, null);
    assert.deepStrictEqual(P.buildFileSpecs('r/x', sel, { name: '' }).errors, [{ code: 'need_name' }]);
    assert.deepStrictEqual(P.buildFileSpecs('r/x', {}, { name: 'n' }).errors, [{ code: 'empty' }]);
    const onlyVae = { 'ae.safetensors': { checked: true, role: 'vae' } };
    assert.deepStrictEqual(P.buildFileSpecs('r/x', onlyVae, { name: 'n' }).errors, [{ code: 'need_diffusion' }]);
});

check('buildFileSpecs: дубликат роли запрещён, но lora можно много', function () {
    const sel = {
        'a.gguf': { checked: true, role: 'diffusion' },
        'b.gguf': { checked: true, role: 'vae' },
        'c.gguf': { checked: true, role: 'vae' },
        'l1.safetensors': { checked: true, role: 'lora' },
        'l2.safetensors': { checked: true, role: 'lora' },
    };
    const built = P.buildFileSpecs('r/x', sel, { name: 'n' });
    assert.strictEqual(built.files.length, 4, 'diffusion + vae + 2 lora');
    assert.deepStrictEqual(built.errors.map(function (e) { return e.code; }), ['dup_role']);
    assert.strictEqual(built.errors[0].role, 'vae');
});

check('buildFileSpecs: неизвестная роль — bad_role', function () {
    const built = P.buildFileSpecs('r/x', { 'x.bin': { checked: true, role: 'nonsense' } }, { name: 'n' });
    assert.deepStrictEqual(built.errors.map(function (e) { return e.code; }), ['bad_role', 'empty']);
});

check('bundleErrorToI18n: коды → ключи переводов', function () {
    assert.strictEqual(P.bundleErrorToI18n({ code: 'need_diffusion' }).key, 'image.bundle_need_diffusion');
    assert.strictEqual(P.bundleErrorToI18n({ code: 'dup_role', role: 'vae' }).vars.role, 'vae');
    assert.strictEqual(P.bundleErrorToI18n({ code: 'empty' }).key, 'imageModels.hf_none_selected');
});

check('progressPercent: completed → 100, иначе из байтов/процента', function () {
    assert.strictEqual(P.progressPercent({ status: 'completed', progressPct: 0 }), 100);
    assert.strictEqual(P.progressPercent({ status: 'downloading', downloaded: 25, totalBytes: 100 }), 25);
    assert.strictEqual(P.progressPercent({ status: 'downloading', progressPct: 42.6 }), 43);
    assert.strictEqual(P.progressPercent({}), 0);
});

check('downloadsSummary: пустой снимок не даёт секций, полный — даёт', function () {
    const empty = P.downloadsSummary({});
    assert.strictEqual(empty.hasAny, false);
    const full = P.downloadsSummary({ bundles: [{ bundleId: 'a' }], history: [{ filename: 'f' }], orphans: [{ filename: 'o' }] });
    assert.strictEqual(full.hasAny, true);
    assert.strictEqual(full.activeCount, 1);
});

// --- пометки по заголовку файла (Phase 10) -----------------------------------

check('repoCompatibility: известные авторы — ok, остальные — честное unknown (без приговоров)', function () {
    assert.strictEqual(P.repoCompatibility({ id: 'leejet/FLUX.1-schnell-gguf' }).level, 'ok');
    assert.strictEqual(P.repoCompatibility({ id: 'QuantStack/Qwen-Image-GGUF' }).level, 'ok');
    // ВАЖНО: ComfyUI-экспорт НЕ приговаривается — движок читает и такую
    // раскладку (проверено на pinned-движке, см. internal/cppbackend/hf_probe.go).
    assert.strictEqual(P.repoCompatibility({ id: 'city96/Qwen-Image-gguf' }).level, 'unknown');
    assert.strictEqual(P.repoCompatibility({ id: 'someone/ComfyUI-GGUF' }).level, 'unknown');
    assert.strictEqual(P.repoCompatibility({}).level, 'unknown', 'пустой id — честное «не знаю»');
});

check('repoCompatibility: у каждого уровня есть подсказка для title', function () {
    ['leejet/x', 'city96/x', 'nobody/x'].forEach(function (id) {
        const b = P.repoCompatibility({ id: id });
        assert.ok(b.label, 'нет подписи у ' + id);
        assert.ok(b.hint, 'нет подсказки у ' + id);
    });
});

check('probeBadge: семья движка в подписи, DiT-подсказка, unknown без приговора', function () {
    const ok = P.probeBadge({ verdict: 'supported', versionLabel: 'Qwen Image 2.1', family: 'qwen_image', dit: true, reason: 'движок узнаёт семейство по тензору "txt_in.text_norm.weight"' });
    assert.strictEqual(ok.level, 'ok');
    assert.ok(ok.label.indexOf('Qwen Image 2.1') !== -1, 'в подписи должно быть имя версии движка: ' + ok.label);
    assert.ok(ok.hint.indexOf('--diffusion-model') !== -1, 'для DiT нужна подсказка про --diffusion-model: ' + ok.hint);
    const unknown = P.probeBadge({ verdict: 'unknown', reason: 'в заголовке нет тензоров, по которым движок узнаёт версию' });
    assert.strictEqual(unknown.level, 'unknown');
    assert.ok(unknown.hint.indexOf('движок узнаёт версию') !== -1, 'причина воркера должна дойти до UI');
    assert.strictEqual(P.probeBadge(null).level, 'unknown', 'нет ответа — не выдумываем вердикт');
    // R86-follow-up: единственный приговор — MLX-кванты (веса U32), их движок не
    // читает. Живой случай 2026-10-06: 4 ГБ скачались, загрузка упала с
    // unsupported dtype "U32" — UI обязан предупредить ДО скачивания.
    const unsupported = P.probeBadge({
        verdict: 'unsupported',
        reason: 'MLX-квантование (тип весов U32 вместе с scales/biases): stable-diffusion.cpp читает GGUF и обычные safetensors (F16/BF16/F8), такой файл не загрузится — нужен GGUF-квант'
    });
    assert.strictEqual(unsupported.level, 'error');
    assert.ok(unsupported.label.indexOf('не поддерживается') !== -1, 'подпись должна быть понятной: ' + unsupported.label);
    assert.ok(unsupported.hint.indexOf('GGUF') !== -1, 'подсказка должна объяснять, что взять: ' + unsupported.hint);
    // Бейдж рисуется красным с иконкой крестика (не «вопросик, как у unknown»).
    const html = P.compatBadgeHtml(unsupported, 'qwen-image-2.1-UC-MLX-4bit.safetensors');
    assert.ok(html.indexOf('fa-circle-xmark') !== -1, 'иконка ошибки: ' + html);
    assert.ok(html.indexOf('--danger') !== -1, 'красный цвет: ' + html);
    assert.ok(html.indexOf('data-imh-probe') !== -1, 'кнопка повторной проверки должна остаться');
});

check('autoProbeTargets: главный файл + крупные safetensors (виновник — не главный)', function () {
    // Раскладка со стенда: главный (самый крупный diffusion) — GGUF с правильными
    // именами, а рядом MLX-safetensors, который движок не читает.
    const files = [
        { path: 'qwen-image-2.1-UC-MLX-4bit.safetensors', sizeBytes: 4002363741, suggestedRole: 'diffusion' },
        { path: 'qwen-image-2.1-UC-Q8_0.gguf', sizeBytes: 2100000000, suggestedRole: 'diffusion' },
        { path: 'qwen_image_2.1_vae_bf16.safetensors', sizeBytes: 675509688, suggestedRole: 'vae' },
        { path: 'README.md', sizeBytes: 1024 }
    ];
    const targets = P.autoProbeTargets(files);
    assert.ok(targets.indexOf('qwen-image-2.1-UC-MLX-4bit.safetensors') !== -1,
        'крупный safetensors обязан проверяться автоматически: ' + targets.join(', '));
    assert.ok(targets.indexOf('qwen_image_2.1_vae_bf16.safetensors') !== -1,
        'второй safetensors (VAE) — тоже кандидат: ' + targets.join(', '));
    assert.ok(targets.indexOf('README.md') === -1, 'не весовые файлы не проверяем');
    assert.ok(targets.length <= 3, 'не больше трёх Range-запросов на репозиторий: ' + targets.length);
    assert.deepStrictEqual(P.autoProbeTargets([]), [], 'пустой список — нечего проверять');
});

check('autoProbeTargets: без safetensors проверяется только главный файл', function () {
    const onlyGGUF = [{ path: 'sd15-q4.gguf', sizeBytes: 1566768416, suggestedRole: 'diffusion' }];
    assert.deepStrictEqual(P.autoProbeTargets(onlyGGUF), ['sd15-q4.gguf']);
});

check('diTFamilyWarning: DiT-файл против all-in-one семейства в профиле', function () {
    const probe = { verdict: 'supported', versionLabel: 'Qwen Image 2.1', family: 'qwen_image', dit: true };
    const warn = P.diTFamilyWarning(probe, 'sd15');
    assert.ok(warn.indexOf('get sd version from file failed') !== -1, 'предупреждение должно называть реальную ошибку движка: ' + warn);
    assert.ok(warn.indexOf('qwen_image') !== -1, 'должно подсказывать правильное семейство');
    assert.ok(P.diTFamilyWarning(probe, 'other').length > 0, 'all-in-one семейство «other» — тоже расхождение');
    assert.strictEqual(P.diTFamilyWarning(probe, 'qwen_image'), '', 'совпало — предупреждения нет');
    // Wan/PixArt мапятся в «other» с dit=true: там семейство уже «как надо».
    assert.strictEqual(P.diTFamilyWarning({ verdict: 'supported', versionLabel: 'Wan', family: 'other', dit: true }, 'other'), '',
        'семейство совпало с определённым — предупреждения нет');
    assert.strictEqual(P.diTFamilyWarning({ verdict: 'supported', versionLabel: 'SD1.x', family: 'sd15', dit: false }, 'other'), '',
        'all-in-one файл от семейства не зависит');
    assert.strictEqual(P.diTFamilyWarning(null, 'sd15'), '', 'нет вердикта — нет предупреждения');
});

check('DIT_FAMILIES совпадает с pkg/types (diTFamilies)', function () {
    assert.deepStrictEqual(P.DIT_FAMILIES.slice().sort(), ['chroma', 'flux', 'flux2', 'qwen_image', 'sd3', 'z_image']);
});

check('mainWeightFile: главный файл — самый крупный DIFFUSION, а не text encoder', function () {
    // Реальный случай (abenzerps/Qwen-Image-2.1-Uncensored-GGUF): TE 17.5 ГБ
    // больше самой модели, и «самый крупный файл» уводил пред-проверку на
    // энкодер — вердикт «не определяется», семейство не подставлялось.
    const files = [
        { path: 'text_encoders/qwen3vl_8b_bf16.safetensors', sizeBytes: 17534334616, suggestedRole: 'llm' },
        { path: 'qwen-image-2.1-UC-BF16.gguf', sizeBytes: 14230272800, suggestedRole: 'diffusion' },
        { path: 'vae/qwen_image_2.1_vae_bf16.safetensors', sizeBytes: 675509688, suggestedRole: 'vae' },
        { path: 'qwen-image-2.1-UC-Q4_0.gguf', sizeBytes: 4151573280, suggestedRole: 'diffusion' },
    ];
    assert.strictEqual(P.mainWeightFile(files).path, 'qwen-image-2.1-UC-BF16.gguf');
    // Нет diffusion-роли (например, файлы ещё не размечены) — берём крупнейший вес.
    const noDiffusion = [
        { path: 'text_encoders/t5xxl.safetensors', sizeBytes: 9000, suggestedRole: 't5xxl' },
        { path: 'vae/ae.safetensors', sizeBytes: 100, suggestedRole: 'vae' },
    ];
    assert.strictEqual(P.mainWeightFile(noDiffusion).path, 'text_encoders/t5xxl.safetensors');
    assert.strictEqual(P.mainWeightFile([]), null);
    assert.strictEqual(P.mainWeightFile([{ path: 'README.md', sizeBytes: 10 }]), null, 'не весовой файл — не главный');
});

check('compatBadgeHtml: кнопка «Проверить» только у файла, причина — в title', function () {
    const withBtn = P.compatBadgeHtml({ level: 'unknown', label: 'не проверено', hint: 'нет данных' }, 'flux1-dev-Q4_0.gguf');
    assert.ok(withBtn.indexOf('data-imh-probe="flux1-dev-Q4_0.gguf"') !== -1, 'нет кнопки пред-проверки: ' + withBtn);
    assert.ok(withBtn.indexOf('title="нет данных"') !== -1, 'причина должна быть в title: ' + withBtn);
    assert.ok(withBtn.indexOf('fa-circle-question') !== -1, 'unknown — серая иконка вопроса');
    const okBadge = P.compatBadgeHtml({ level: 'ok', label: 'движок узнаёт: Flux', hint: '' }, '');
    assert.ok(okBadge.indexOf('fa-circle-check') !== -1, 'ok — зелёная галочка');
    assert.strictEqual(okBadge.indexOf('data-imh-probe'), -1, 'без пути кнопки быть не должно');
    assert.strictEqual(P.compatBadgeHtml(null, '').indexOf('undefined'), -1, 'пустой вердикт не должен течь в разметку');
});

check('ditHint: flux без VAE/text encoder предупреждает, полный набор и SD1.5 — нет', function () {
    const only = [{ path: 'flux1-dev-Q4_0.gguf', suggestedRole: 'diffusion' }];
    const warn = P.ditHint(only, 'leejet/FLUX.1-dev-gguf');
    assert.ok(warn.indexOf('VAE') !== -1, 'нет упоминания VAE: ' + warn);
    assert.ok(warn.indexOf('text encoder') !== -1, 'нет упоминания text encoder: ' + warn);
    const full = [
        { path: 'flux1-dev-Q4_0.gguf', suggestedRole: 'diffusion' },
        { path: 'ae.safetensors', suggestedRole: 'vae' },
        { path: 'clip_l.safetensors', suggestedRole: 'clip_l' },
    ];
    assert.strictEqual(P.ditHint(full, 'leejet/FLUX.1-dev-gguf'), '', 'полный набор — предупреждения нет');
    assert.strictEqual(P.ditHint(only, 'sd15-model'), '', 'SD1.5 не DiT: отдельные VAE/TE не нужны');
    assert.strictEqual(P.ditHint(only, 'qwen-image-2.1-uncensored-gguf').length > 0, true, 'qwen — тоже DiT-семейство');
});

check('модуль не рендерит сгенерированные картинки (base64/img)', function () {
    const src = fs.readFileSync(path.join(__dirname, 'image-models-hf.js'), 'utf8');
    ['b64_json', '<img', 'data:image', '/api/image/generate', 'imgResults', 'gallery'].forEach(function (tok) {
        assert.strictEqual(src.indexOf(tok), -1, 'в модуле не должно быть ' + tok);
    });
});

// ============================================================================
// 2. Асинхронные сценарии (мок fetch)
// ============================================================================

(async function run() {
    Hf.mount();
    Hf.render(ctx);

    // --- поиск --------------------------------------------------------------
    store['ollamalegion_hf_token'] = 'hf_secret';
    responseFor = function (rec) {
        if (rec.url.indexOf('/hf/search') !== -1) {
            return { status: 200, body: { results: [{ id: 'leejet/FLUX.1-schnell-gguf', downloads: 1200, likes: 30, pipelineTag: 'text-to-image' }], count: 1 } };
        }
        if (rec.url.indexOf('/hf/files') !== -1) return { status: 200, body: { files: FILES, count: FILES.length } };
        // Пред-проверка заголовка: главный файл (t5xxl) — DiT-семейство qwen_image,
        // любой другой — «по заголовку не определить» (как VAE/text encoder).
        if (rec.url.indexOf('/hf/probe') !== -1) {
            // Главный файл репозитория — самый крупный DIFFUSION-файл (t5xxl
            // крупнее, но он text encoder: его пред-проверка не главная).
            if (rec.url.indexOf('flux1-schnell-Q4_0.gguf') !== -1) {
                return {
                    status: 200, body: {
                        verdict: 'supported', family: 'qwen_image', versionLabel: 'Qwen Image 2.1', dit: true,
                        reason: 'движок узнаёт семейство по тензору "txt_in.text_norm.weight"', sizeBytes: 5000000000,
                    },
                };
            }
            return {
                status: 200, body: {
                    verdict: 'unknown', dit: false,
                    reason: 'в заголовке нет тензоров, по которым движок узнаёт версию модели (возможно, это VAE/text encoder/LoRA)',
                },
            };
        }
        if (rec.url.indexOf('/hf/progress') !== -1) {
            return { status: 200, body: { bundleId: 'flux-schnell-q4', status: 'completed', progressPct: 100, registered: true, files: [] } };
        }
        if (rec.url.indexOf('/hf/downloads') !== -1) {
            return { status: 200, body: { active: [], history: [], orphans: [], bundles: [], bundleHistory: [] } };
        }
        if (rec.url.indexOf('/hf/bundle') !== -1) return { status: 202, body: { status: 'started', bundleId: 'flux-schnell-q4' } };
        if (rec.url.indexOf('/hf/cleanup') !== -1) return { status: 200, body: { status: 'deleted' } };
        return { status: 404, body: { error: 'unexpected ' + rec.url } };
    };

    getEl('imHfQuery').value = 'FLUX.1-schnell';
    getEl('imHfTask').value = 'text-to-image';
    await Hf._actions.search();
    await sleep(10);

    check('поиск: GET /hf/search с query/limit/task через балансер', function () {
        const rec = requests.filter(function (r) { return r.url.indexOf('/hf/search') !== -1; })[0];
        assert.ok(rec, 'запрос поиска не ушёл');
        assert.strictEqual(rec.url.indexOf('http://balancer.test:18081/api/v1/image/backends/img-1/hf/search'), 0, 'неверный путь: ' + rec.url);
        assert.ok(rec.url.indexOf('query=FLUX.1-schnell') !== -1, 'нет query: ' + rec.url);
        assert.ok(rec.url.indexOf('limit=20') !== -1, 'нет limit');
        assert.ok(rec.url.indexOf('task=text-to-image') !== -1, 'нет фильтра задачи');
    });
    check('поиск: HF-токен уходит в X-HF-Token, авторизация — в X-API-Token', function () {
        const rec = requests.filter(function (r) { return r.url.indexOf('/hf/search') !== -1; })[0];
        assert.strictEqual(rec.headers['X-HF-Token'], 'hf_secret');
        assert.strictEqual(rec.headers['X-API-Token'], 'test-token');
    });
    check('результаты поиска отрисованы карточками (id репозитория + загрузки)', function () {
        const html = getEl('imHfSearchResults').innerHTML;
        assert.ok(html.indexOf('leejet/FLUX.1-schnell-gguf') !== -1, 'нет карточки репозитория: ' + html.slice(0, 200));
        assert.ok(html.indexOf('gguf-result-card') !== -1, 'нет стиля карточек GGUF');
    });

    // --- файлы репозитория ---------------------------------------------------
    await Hf._actions.pickRepo('leejet/FLUX.1-schnell-gguf');
    await sleep(10);

    check('файлы: GET /hf/files?modelId=..., роль из suggestedRole (в JS нет своей таблицы ролей)', function () {
        const rec = requests.filter(function (r) { return r.url.indexOf('/hf/files') !== -1; })[0];
        assert.ok(rec, 'запрос файлов не ушёл');
        assert.ok(rec.url.indexOf('modelId=leejet%2FFLUX.1-schnell-gguf') !== -1, 'нет modelId: ' + rec.url);
        const html = getEl('imHfFilesBody').innerHTML;
        assert.ok(html.indexOf('t5xxl-Q4_K_M.gguf') !== -1, 'нет файла t5xxl в списке');
        assert.ok(html.indexOf('data-imh-role="t5xxl-Q4_K_M.gguf"') !== -1, 'нет селекта роли для t5xxl');
    });
    check('отметки: diffusion/vae/clip_l — checked, t5xxl — нет', function () {
        const sel = Hf._state.selection;
        assert.strictEqual(sel['flux1-schnell-Q4_0.gguf'].checked, true);
        assert.strictEqual(sel['t5xxl-Q4_K_M.gguf'].checked, false);
        assert.ok(getEl('imHfSelectionSummary').textContent.indexOf('3') !== -1,
            'в сводке должно быть 3 выбранных файла: ' + getEl('imHfSelectionSummary').textContent);
    });
    check('имя bundle по умолчанию подставлено из имени репозитория', function () {
        assert.strictEqual(getEl('imgBundleName').value, 'flux.1-schnell-gguf');
        assert.ok(getEl('imgBundleFamily').innerHTML.indexOf('flux') !== -1, 'селект семейств не заполнен');
    });

    // --- пометки по заголовку и пред-проверка файла --------------------------
    check('пред-проверка: главный (самый крупный) файл проверяется сразу после выбора репозитория', function () {
        const rec = requests.filter(function (r) { return r.url.indexOf('/hf/probe') !== -1; })[0];
        assert.ok(rec, 'GET /hf/probe не ушёл после pickRepo');
        assert.ok(rec.url.indexOf('/api/v1/image/backends/img-1/hf/probe') !== -1, 'неверный путь: ' + rec.url);
        assert.ok(rec.url.indexOf('modelId=leejet%2FFLUX.1-schnell-gguf') !== -1, 'нет modelId: ' + rec.url);
        assert.strictEqual(rec.headers['X-HF-Token'], 'hf_secret', 'HF-токен нужен и пред-проверке (приватные репозитории)');
        assert.ok(rec.url.indexOf('filename=flux1-schnell-Q4_0.gguf') !== -1, 'проверяться должен главный diffusion-файл: ' + rec.url);
    });
    check('пред-проверка: вердикт воркера сохранён и отрисован у строки файла', function () {
        const probe = Hf._state.probes['flux1-schnell-Q4_0.gguf'];
        assert.ok(probe && probe.verdict === 'supported', 'вердикт не сохранён: ' + JSON.stringify(probe));
        const html = getEl('imHfFilesBody').innerHTML;
        assert.ok(html.indexOf('движок узнаёт: Qwen Image 2.1') !== -1, 'нет вердикта у файла: ' + html.slice(0, 400));
    });
    check('DiT-файл автоподставляет семейство в профиле (иначе движок ответит get sd version…)', function () {
        assert.strictEqual(Hf._state.bundleFamily, 'qwen_image', 'семейство должно быть подставлено по заголовку файла');
        assert.strictEqual(getEl('imgBundleFamily').value, 'qwen_image', 'селект семейства не обновлён');
        assert.ok(getEl('imHfFilesBody').innerHTML.indexOf('Семейство профиля переключено') !== -1,
            'оператор должен видеть, почему семейство изменилось');
    });
    check('шапка файлов: оценка репозитория есть, а DiT-предупреждения нет (VAE и clip_l в наборе)', function () {
        const html = getEl('imHfFilesBody').innerHTML;
        assert.ok(html.indexOf('Оценка репозитория') !== -1, 'нет строки с оценкой репозитория');
        assert.ok(html.indexOf('автор публикует сборки под sd.cpp') !== -1, 'leejet/ — известный автор сборок под sd.cpp');
        assert.strictEqual(html.indexOf('Похоже на семейство'), -1,
            'в наборе есть vae и clip_l: предупреждать не о чем');
        assert.strictEqual(html.indexOf('а в профиле выбрано'), -1,
            'семейство подставлено — предупреждения о расхождении быть не должно');
    });
    check('семейство, выбранное оператором вручную, запоминается', function () {
        getEl('image-page').dispatch('change', { target: { id: 'imgBundleFamily', value: 'sd15', getAttribute: function () { return null; } } });
        assert.strictEqual(Hf._state.bundleFamily, 'sd15');
        assert.strictEqual(Hf._state.familyManual, true, 'ручной выбор должен запомниться');
    });
    check('ручной выбор all-in-one семейства сразу показывает предупреждение о расхождении', function () {
        const html = getEl('imHfFilesBody').innerHTML;
        assert.ok(html.indexOf('а в профиле выбрано') !== -1,
            'смена семейства должна перерисовывать шапку с предупреждением: ' + html.slice(0, 200));
    });
    await Hf._actions.probeFile('ae.safetensors');
    check('ручной выбор семейства автоподстановкой не перебивается', function () {
        assert.strictEqual(Hf._state.bundleFamily, 'sd15', 'после ручного выбора семейство не подставляется');
    });
    // Возвращаем DiT-семейство, чтобы следующие проверки шли по «правильному» пути.
    getEl('image-page').dispatch('change', { target: { id: 'imgBundleFamily', value: 'flux', getAttribute: function () { return null; } } });

    // Клик по кнопке «Проверить» (делегирование на #image-page) проверяет ДРУГОЙ
    // файл — и вердикт у него другой: так ловится подмена пути в data-атрибуте.
    requests.length = 0;
    getEl('image-page').dispatch('click', {
        target: {
            getAttribute: function (n) { return n === 'data-imh-probe' ? 'clip_l.safetensors' : null; },
        },
    });
    await sleep(10);
    check('клик по data-imh-probe уходит в probeFile с этим файлом', function () {
        const rec = requests.filter(function (r) { return r.url.indexOf('/hf/probe') !== -1; })[0];
        assert.ok(rec, 'клик по кнопке не вызвал пред-проверку');
        assert.ok(rec.url.indexOf('filename=clip_l.safetensors') !== -1, 'проверен не тот файл: ' + rec.url);
        const probe = Hf._state.probes['clip_l.safetensors'];
        assert.strictEqual(probe && probe.verdict, 'unknown');
        const html = getEl('imHfFilesBody').innerHTML;
        assert.ok(html.indexOf('версия по заголовку не определяется') !== -1, 'вердикт «не определяется» не отрисован');
        assert.ok(html.indexOf('text encoder') !== -1, 'причина воркера не показана');
    });

    // --- сборка и запуск bundle ---------------------------------------------
    getEl('imgBundleName').value = 'flux-schnell-q4';
    getEl('imgBundleFamily').value = 'flux';
    getEl('imgHfToken').value = 'hf_new';
    requests.length = 0;
    await Hf._actions.startBundle();
    await sleep(10);

    check('bundle: POST /hf/bundle {name, family, files[]} и HF-токен сохранён', function () {
        const rec = requests.filter(function (r) { return r.url.indexOf('/hf/bundle') !== -1; })[0];
        assert.ok(rec, 'POST /hf/bundle не ушёл');
        assert.strictEqual(rec.method, 'POST');
        assert.ok(rec.url.indexOf('/api/v1/image/backends/img-1/hf/bundle') !== -1, 'неверный путь: ' + rec.url);
        assert.strictEqual(rec.body.name, 'flux-schnell-q4');
        assert.strictEqual(rec.body.family, 'flux');
        assert.strictEqual(rec.body.files.length, 3, 'diffusion + vae + clip_l');
        assert.strictEqual(rec.body.files[0].role, 'diffusion');
        assert.strictEqual(store['ollamalegion_hf_token'], 'hf_new');
    });
    check('прогресс: опрос /hf/progress?bundleId= и остановка на финальном статусе', function () {
        const rec = requests.filter(function (r) { return r.url.indexOf('/hf/progress') !== -1; })[0];
        assert.ok(rec, 'опрос прогресса не пошёл');
        assert.ok(rec.url.indexOf('bundleId=flux-schnell-q4') !== -1, 'нет bundleId: ' + rec.url);
        assert.strictEqual(Hf._state.poll, null, 'поллинг должен остановиться на status=completed');
        assert.strictEqual(getEl('imgBundleProgress').style.display, '', 'блок прогресса должен быть показан');
    });
    check('после завершения список моделей перечитан (новая модель видна на «Моделях на диске»)', function () {
        assert.ok(refreshCalls >= 1, 'ImagePage._actions.refresh не вызван');
    });
    Hf._actions.stopPolling();

    // --- ошибка сборки -------------------------------------------------------
    requests.length = 0;
    Hf._actions.clearSelection();
    await Hf._actions.startBundle();
    check('пустой выбор: запрос не уходит, тост об ошибке', function () {
        assert.strictEqual(requests.filter(function (r) { return r.url.indexOf('/hf/bundle') !== -1; }).length, 0);
        assert.ok(toasts.some(function (x) { return x.msg.indexOf('хотя бы один файл') !== -1; }), 'нет тоста о пустом выборе');
    });

    // --- «Загрузки» ----------------------------------------------------------
    responseFor = function (rec) {
        if (rec.url.indexOf('/hf/downloads') !== -1) {
            return {
                status: 200, body: {
                    active: [{ modelId: 'a/b', filename: 'x.gguf', status: 'downloading', downloaded: 5, totalBytes: 10 }],
                    history: [{ modelId: 'a/b', filename: 'y.gguf', status: 'completed', downloaded: 10, totalBytes: 10 }],
                    orphans: [{ filename: 'flux/ae.safetensors', size: 1024, path: '/models/downloads/flux/ae.safetensors.download' }],
                    bundles: [{ bundleId: 'z-image', status: 'downloading', downloaded: 1, totalBytes: 4, files: [{ role: 'diffusion', filename: 'd.gguf', status: 'downloading' }] }],
                    bundleHistory: [{ bundleId: 'sd15', status: 'completed', progressPct: 100, registered: true, files: [] }],
                },
            };
        }
        if (rec.url.indexOf('/hf/cleanup') !== -1) return { status: 200, body: { status: 'deleted' } };
        return { status: 404, body: { error: 'unexpected' } };
    };
    requests.length = 0;
    Hf.render({ apiBase: ctx.apiBase, headers: ctx.headers, backendId: 'img-1', tab: 'downloads' });
    await sleep(10);

    check('«Загрузки»: активные bundle, активные файлы, история и остатки в одном снимке', function () {
        const html = getEl('imDownloadsHost').innerHTML;
        assert.ok(html.indexOf('z-image') !== -1, 'нет активного bundle');
        assert.ok(html.indexOf('Загрузка...') !== -1, 'нет статуса загрузки');
        assert.ok(html.indexOf('x.gguf') !== -1, 'нет активного файла');
        assert.ok(html.indexOf('История загрузок') !== -1, 'нет секции истории');
        assert.ok(html.indexOf('sd15') !== -1, 'нет bundle из истории');
        assert.ok(html.indexOf('Остаточные файлы') !== -1, 'нет секции остатков');
        assert.ok(html.indexOf('flux/ae.safetensors') !== -1, 'нет имени остаточного файла');
    });
    check('«Загрузки»: секции не дублируются (одна секция на вид загрузки)', function () {
        const html = getEl('imDownloadsHost').innerHTML;
        assert.strictEqual((html.match(/Активные загрузки файлов/g) || []).length, 1, 'секция активных файлов должна быть одна');
        assert.strictEqual((html.match(/История загрузок/g) || []).length, 1, 'секция истории должна быть одна');
        assert.strictEqual((html.match(/Остаточные файлы/g) || []).length, 1, 'секция остатков должна быть одна');
    });

    requests.length = 0;
    await Hf._actions.cleanupOrphan('flux/ae.safetensors');
    await sleep(10);
    check('остаток: DELETE /hf/cleanup?filename=... (имя с «/» в query, а не в пути)', function () {
        const rec = requests.filter(function (r) { return r.url.indexOf('/hf/cleanup') !== -1; })[0];
        assert.ok(rec, 'запрос очистки не ушёл');
        assert.strictEqual(rec.method, 'DELETE');
        assert.ok(rec.url.indexOf('filename=flux%2Fae.safetensors') !== -1, 'нет filename в query: ' + rec.url);
    });

    console.log('\n' + (failures.length ? 'FAILED: ' + failures.length : 'OK: ' + passed + ' checks passed'));
    if (failures.length) {
        failures.forEach(function (f) { console.error(' - ' + f); });
        process.exit(1);
    }
})();

check('planHtml: режим, комплектность, шаги и подсказка модели', function () {
    // Паспорт неполного DiT-набора (живой случай Qwen-Image 2.1: только diffusion).
    const plan = {
        verdict: 'incomplete',
        summary: 'Движок запустит как DiT (--diffusion-model + отдельные VAE/text encoder), но не хватает: VAE, text encoder (LLM).',
        engineMode: 'diffusion-model',
        roles: [
            { role: 'diffusion', label: 'diffusion (DiT)', required: true, found: true, files: ['qwen-image-2.1-UC-Q4_K_M.gguf'] },
            { role: 'vae', label: 'VAE', required: true, found: false, hint: 'Нужен отдельный VAE' },
            { role: 'llm', label: 'text encoder (LLM)', required: true, found: false, hint: 'Нужен отдельный text encoder' },
            { role: 'lora', label: 'LoRA', required: false, found: false }
        ],
        steps: ['Отметьте файл qwen-image-2.1-UC-Q4_K_M.gguf.', 'В поле «Семейство» выберите qwen_image.']
    };
    const html = P.planHtml(plan);
    assert.ok(html.indexOf('--diffusion-model') !== -1, 'режим движка виден: ' + html);
    assert.ok(html.indexOf('VAE') !== -1 && html.indexOf('text encoder') !== -1, 'обязательные роли перечислены');
    assert.ok(html.indexOf('❌') !== -1, 'отсутствующие роли помечены');
    assert.ok(html.indexOf('Нужен отдельный VAE') !== -1, 'подсказка, что искать');
    assert.ok(html.indexOf('qwen_image') !== -1, 'шаг про семейство');
    assert.ok(html.indexOf('LoRA') === -1, 'необязательные роли не засоряют паспорт');
    assert.ok(html.indexOf('--danger') === -1 && html.indexOf('e0a030') !== -1, 'неполный набор — предупреждение');
});

check('planHtml: пустой паспорт и режим all-in-one', function () {
    assert.strictEqual(P.planHtml(null), '', 'без паспорта разметки нет');
    const ready = P.planHtml({
        verdict: 'ready', summary: 'Набор собран: движок узнаёт «SD1.x» и запустит как all-in-one (--model).',
        engineMode: 'model', roles: [{ role: 'diffusion', label: 'diffusion (all-in-one)', required: true, found: true, files: ['sd15.gguf'] }],
        steps: [], modelHint: 'Инструменту generate_image передавайте model="sd15-q4".'
    });
    assert.ok(ready.indexOf('--model') !== -1, 'all-in-one режим: ' + ready);
    assert.ok(ready.indexOf('fa-robot') !== -1, 'подсказка модели показана');
    assert.ok(ready.indexOf('e0a030') === -1, 'готовый набор без предупреждения');
});