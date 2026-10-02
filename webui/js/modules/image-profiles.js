/**
 * image-profiles.js - редактор профилей image-моделей (план §5.7, R-Image Phase 6).
 *
 * ЧТО ЭТО: UI для CRUD профилей bundle'ов stable-diffusion.cpp, по образцу
 * webui/js/modules/cppworker-params.js (modal-wizard на .mode-wizard-overlay).
 *
 * API балансера (заголовок X-API-Token, порт управления 18081):
 *   GET    /api/v1/image/model-profiles                       -> {models:{name:profile},total}
 *   GET    /api/v1/image/model-profiles/{name}                -> {model,profile,serverArgs}
 *   PUT    /api/v1/image/model-profiles/{name}                -> {status,model,profile}
 *   DELETE /api/v1/image/model-profiles/{name}
 *   POST   /api/v1/image/model-profiles/{name}/apply          -> {applyId,status,backends[]}
 *   GET    /api/v1/image/model-profiles/{name}/apply/progress -> то же (или 404)
 *   GET    /api/v1/image/model-profiles/{name}/apply/status/{id}
 *   GET    /api/v1/image/model-catalog                        -> {presets:[profile],total}
 *   GET    /api/v1/image/backends                             -> бэкенды (для выбора цели)
 *
 * ВАЖНОЕ ПРО PUT: сервер мержит обновление PATCH-like (см. mergeImageProfileUpdate
 * в internal/api/handlers_image_profiles.go): нулевые числа и пустые строки
 * означают «не менять». Полный профиль из формы безопасен, но обнулить поле
 * через UI нельзя - и это честно сказано в подсказке редактора. Булевы флаги и
 * seed сервер различает по presence, поэтому их мы шлём ВСЕГДА.
 *
 * Apply для image-моделей - это «сохранить профиль + проверить доступность
 * воркера»: sd-server поднимается воркером при загрузке модели, push-эндпоинта
 * профилей в контракте воркера нет. Поэтому статус бэкенда пробрасываем как
 * есть (persisted / unreachable / error) и при unreachable пишем честный
 * warning, а не «успех» (internal/api/handlers_image_profiles.go:363-452).
 *
 * Зависимости: window.I18N, window.Api.getAuthHeaders (как в image-page.js).
 * Экспорт: window.ImageProfiles = { mount, refresh, pure, _state, _actions }.
 */
(function () {
    'use strict';

    // ---- Контракт pkg/types/image_model.go (заморожен, не менять в одиночку) ----
    var IMAGE_ROLES = ['diffusion', 'vae', 'clip_l', 'clip_g', 't5xxl', 'llm', 'clip_vision', 'taesd', 'lora', 'upscaler', 'controlnet', 'ip_adapter'];
    var IMAGE_FAMILIES = ['sd15', 'sd21', 'sd_turbo', 'sdxl', 'sdxl_turbo', 'sd3', 'flux', 'flux2', 'chroma', 'qwen_image', 'z_image', 'other'];
    // DiT-семейства: diffusion/VAE/text-encoder лежат отдельными файлами
    // (types.diTFamilies), для них VAE обязателен отдельным файлом.
    var DIT_FAMILIES = ['sd3', 'flux', 'flux2', 'chroma', 'qwen_image', 'z_image'];
    var SAMPLER_SUGGESTIONS = ['euler', 'euler_a', 'dpm++2m', 'dpm++2s_a', 'lcm', 'ddim_trailing', 'heun', 'dpm2'];
    var SCHEDULER_SUGGESTIONS = ['discrete', 'karras', 'exponential', 'ays', 'gits', 'smoothstep'];
    // Роли, которые в форме показываем чаще остальных (для сброса/подсказки).
    var ROLE_ORDER = ['diffusion', 'vae', 'clip_l', 'clip_g', 't5xxl', 'llm', 'clip_vision', 'taesd', 'lora', 'upscaler', 'controlnet', 'ip_adapter'];

    var LIMITS = {
        minSteps: 1, maxSteps: 100,
        minCfg: 0, maxCfg: 30,
        minSide: 64, maxSide: 4096, sideStep: 64,
        minBatch: 1, maxBatch: 8,
        minSeed: -1,
        minGpuLayers: -1, maxGpuLayers: 999
    };

    var TIMEOUT_CONTROL_MS = 15000;
    var APPLY_POLL_MS = 1200;
    var APPLY_POLL_MAX = 40;

    var PATH_PROFILES = '/api/v1/image/model-profiles';
    var PATH_CATALOG = '/api/v1/image/model-catalog';
    var PATH_BACKENDS = '/api/v1/image/backends';
    var PATH_BACKENDS_FALLBACK = '/api/v1/backends';

    // =====================================================================
    // Pure helpers (без DOM) - покрыты webui/js/modules/image-profiles.test.js
    // =====================================================================

    function escapeHtml(text) {
        if (text === null || text === undefined) return '';
        return String(text)
            .replace(/&/g, '&amp;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;')
            .replace(/'/g, '&#39;');
    }

    function isDitFamily(family) {
        return DIT_FAMILIES.indexOf(String(family || '')) >= 0;
    }

    function isValidFamily(family) {
        return IMAGE_FAMILIES.indexOf(String(family || '')) >= 0;
    }

    function isValidRole(role) {
        return IMAGE_ROLES.indexOf(String(role || '')) >= 0;
    }

    /** Байты -> человекочитаемо (1024-шкала, как image-page.formatBytes). */
    function formatBytes(n) {
        if (n === null || n === undefined || n === '' || isNaN(Number(n))) return '-';
        var v = Number(n);
        if (v <= 0) return '0 B';
        var units = ['B', 'KB', 'MB', 'GB', 'TB'];
        var i = 0;
        while (v >= 1024 && i < units.length - 1) { v = v / 1024; i++; }
        return (i === 0 ? String(Math.round(v)) : v.toFixed(v >= 100 ? 0 : 1)) + ' ' + units[i];
    }

    function formatDuration(ms) {
        if (!ms || ms < 0 || isNaN(Number(ms))) return '0s';
        var sec = Math.floor(Number(ms) / 1000);
        if (sec < 60) return sec + 's';
        var min = Math.floor(sec / 60);
        var s = sec % 60;
        if (min < 60) return min + 'm ' + (s < 10 ? '0' + s : s) + 's';
        var h = Math.floor(min / 60);
        var m = min % 60;
        return h + 'h ' + (m < 10 ? '0' + m : m) + 'm';
    }

    function clampInt(value, min, max, def) {
        var v = parseInt(value, 10);
        if (isNaN(v)) return def;
        if (v < min) return min;
        if (v > max) return max;
        return v;
    }

    /** Сумма sizeBytes по файлам профиля (0, если размеры неизвестны). */
    function bundleSizeBytes(files) {
        var total = 0;
        (files || []).forEach(function (f) {
            var n = Number((f && (f.sizeBytes || f.size_bytes)) || 0);
            if (n > 0) total += n;
        });
        return total;
    }

    /** Рекомендованные дефолты семейства (types.DefaultImageGenDefaults). */
    function familyDefaultsFor(family) {
        var base = {
            steps: 20, cfgScale: 7.0, sampler: 'euler_a', scheduler: 'discrete',
            width: 512, height: 512, batchCount: 1, seed: -1, clipSkip: 0
        };
        switch (String(family || '')) {
        case 'sd_turbo':
            base.steps = 4; base.cfgScale = 1.0; base.sampler = 'euler';
            break;
        case 'sdxl_turbo':
            base.steps = 4; base.cfgScale = 1.0; base.sampler = 'euler';
            base.width = 512; base.height = 512;
            break;
        case 'sd15':
        case 'sd21':
            base.steps = 25;
            break;
        case 'sdxl':
            base.steps = 25; base.width = 1024; base.height = 1024;
            break;
        case 'sd3':
            base.steps = 28; base.cfgScale = 4.5; base.sampler = 'euler';
            base.width = 1024; base.height = 1024;
            break;
        case 'flux':
        case 'flux2':
            base.steps = 4; base.cfgScale = 1.0; base.sampler = 'euler';
            base.width = 1024; base.height = 1024;
            break;
        case 'chroma':
            base.cfgScale = 4.0; base.sampler = 'euler';
            base.width = 1024; base.height = 1024;
            break;
        case 'qwen_image':
            base.steps = 25; base.cfgScale = 2.5; base.sampler = 'euler';
            base.width = 1024; base.height = 1024;
            break;
        case 'z_image':
            base.steps = 8; base.cfgScale = 1.0; base.sampler = 'euler';
            base.scheduler = 'smoothstep'; base.width = 512; base.height = 1024;
            break;
        default:
            break;
        }
        return base;
    }

    /**
     * Профиль API -> плоское состояние формы.
     *
     * Вложенные объекты разворачиваются в плоские ключи (defaults.steps ->
     * defaultsSteps, runtime.vaeTiling -> runtimeVaeTiling), потому что форма
     * читается из DOM по именам полей. extraArgs остаётся массивом, а рендер
     * кладёт его в textarea построчно.
     */
    function profileToForm(profile, seedDefaults) {
        profile = profile || {};
        var family = String(profile.family || '');
        var d = profile.defaults || {};
        var r = profile.runtime || {};
        var fam = familyDefaultsFor(family);
        var useDefaults = !!seedDefaults;

        function pickNum(value, fallback) {
            if (value === null || value === undefined || value === '') {
                return useDefaults ? fallback : '';
            }
            return value;
        }

        return {
            name: String(profile.name || ''),
            family: family,
            notes: String(profile.notes || ''),
            disabled: !!profile.disabled,
            vramEstimateMb: pickNum(profile.vramEstimateMb, 0),
            timeoutSec: pickNum(profile.timeoutSec, 0),
            idleUnloadMinutes: pickNum(profile.idleUnloadMinutes, 0),

            defaultsSteps: pickNum(d.steps, fam.steps),
            defaultsCfgScale: pickNum(d.cfgScale, fam.cfgScale),
            defaultsSampler: (useDefaults && !d.sampler) ? fam.sampler : String(d.sampler || ''),
            defaultsScheduler: (useDefaults && !d.scheduler) ? fam.scheduler : String(d.scheduler || ''),
            defaultsWidth: pickNum(d.width, fam.width),
            defaultsHeight: pickNum(d.height, fam.height),
            defaultsBatchCount: pickNum(d.batchCount, fam.batchCount),
            defaultsNegativePrompt: String(d.negativePrompt || ''),
            defaultsSeed: pickNum(d.seed, -1),
            defaultsClipSkip: pickNum(d.clipSkip, 0),

            runtimeBackend: String(r.backend || ''),
            runtimeParamsBackend: String(r.paramsBackend || ''),
            runtimeMaxVram: String(r.maxVram || ''),
            runtimeOffloadToCpu: !!r.offloadToCpu,
            runtimeAutoFit: String(r.autoFit || ''),
            runtimeDiffusionFa: !!r.diffusionFa,
            runtimeVaeTiling: !!r.vaeTiling,
            runtimeVaeTileSize: pickNum(r.vaeTileSize, 0),
            runtimeVaeConvDirect: !!r.vaeConvDirect,
            runtimeTaesd: !!r.taesd,
            runtimeSplitMode: String(r.splitMode || ''),
            runtimeNGpuLayers: pickNum(r.nGpuLayers, 0),
            runtimeThreads: pickNum(r.threads, 0),
            runtimeSeedMode: String(r.seedMode || 'random'),
            runtimeExtraArgs: Array.isArray(r.extraArgs) ? r.extraArgs.slice() : [],

            files: (profile.files || []).map(function (f) {
                f = f || {};
                return {
                    role: String(f.role || 'diffusion'),
                    repo: String(f.repo || ''),
                    filename: String(f.filename || ''),
                    revision: String(f.revision || 'main') || 'main',
                    sizeBytes: Number(f.sizeBytes || 0),
                    localPath: String(f.localPath || '')
                };
            })
        };
    }

    /** Плоские строки формы -> files[] профиля (пустые строки отбрасываются). */
    function filesFromForm(form) {
        var out = [];
        ((form && form.files) || []).forEach(function (f) {
            f = f || {};
            var role = String(f.role || '').trim();
            var repo = String(f.repo || '').trim();
            var filename = String(f.filename || '').trim();
            if (!role && !repo && !filename) return;
            out.push({
                role: role,
                repo: repo,
                filename: filename,
                revision: String(f.revision || 'main').trim() || 'main'
            });
        });
        return out;
    }

    /**
     * Плоское состояние формы -> тело PUT.
     *
     * Числовые поля: пустая строка = «сервер подставит дефолт семейства»
     * (сервер мержит нули как «не менять»), поэтому пустое поле НЕ отправляем.
     * Булевы и seed отправляем всегда: сервер различает их по presence, иначе
     * выключить флаг (offloadToCpu: false) было бы невозможно.
     * runtime.extraArgs[] отправляем как есть (пустой список тоже: nil = не менять).
     */
    function formToProfile(form, opts) {
        form = form || {};
        opts = opts || {};
        var base = opts.baseProfile || {};
        var family = String(form.family || base.family || 'other');
        var fam = familyDefaultsFor(family);
        var out = {
            name: String(form.name || base.name || '').trim(),
            family: family,
            files: filesFromForm(form),
            defaults: {},
            runtime: {}
        };

        function num(field) {
            var raw = form[field];
            if (raw === null || raw === undefined) return null;
            var s = String(raw).trim();
            if (s === '') return null;
            var n = parseFloat(s);
            return isNaN(n) ? null : n;
        }
        function int(field) {
            var n = num(field);
            return n === null ? null : Math.round(n);
        }

        var d = out.defaults;
        var steps = int('defaultsSteps');
        d.steps = steps === null ? fam.steps : steps;
        var cfg = num('defaultsCfgScale');
        d.cfgScale = cfg === null ? fam.cfgScale : cfg;
        d.sampler = String(form.defaultsSampler || '').trim() || fam.sampler;
        d.scheduler = String(form.defaultsScheduler || '').trim() || fam.scheduler;
        var w = int('defaultsWidth');
        d.width = w === null ? fam.width : w;
        var h = int('defaultsHeight');
        d.height = h === null ? fam.height : h;
        var batch = int('defaultsBatchCount');
        d.batchCount = batch === null ? fam.batchCount : batch;
        var seed = int('defaultsSeed');
        d.seed = seed === null ? -1 : seed;
        var clipSkip = int('defaultsClipSkip');
        d.clipSkip = clipSkip === null ? 0 : clipSkip;
        var neg = String(form.defaultsNegativePrompt || '');
        if (neg) d.negativePrompt = neg;
        out.defaults = d;

        var r = out.runtime;
        r.backend = String(form.runtimeBackend || '').trim();
        r.paramsBackend = String(form.runtimeParamsBackend || '').trim();
        r.maxVram = String(form.runtimeMaxVram || '').trim();
        r.offloadToCpu = !!form.runtimeOffloadToCpu;
        r.autoFit = String(form.runtimeAutoFit || '').trim();
        r.diffusionFa = !!form.runtimeDiffusionFa;
        r.vaeTiling = !!form.runtimeVaeTiling;
        r.vaeTileSize = int('runtimeVaeTileSize') || 0;
        r.vaeConvDirect = !!form.runtimeVaeConvDirect;
        r.taesd = !!form.runtimeTaesd;
        r.splitMode = String(form.runtimeSplitMode || '').trim();
        r.nGpuLayers = int('runtimeNGpuLayers') || 0;
        r.threads = int('runtimeThreads') || 0;
        r.seedMode = String(form.runtimeSeedMode || 'random').trim() || 'random';
        var extra = form.runtimeExtraArgs;
        if (typeof extra === 'string') extra = splitArgsText(extra);
        // Пустые строки отбрасываем всегда: textarea легко даёт "\n\n", а
        // sd-server получил бы пустой аргумент в argv.
        r.extraArgs = (Array.isArray(extra) ? extra : []).filter(function (a) {
            return String(a === null || a === undefined ? '' : a).trim() !== '';
        }).slice();
        out.runtime = r;

        // Числовые поля верхнего уровня: пусто/0 = «оставить как есть».
        var vram = int('vramEstimateMb');
        if (vram !== null && vram > 0) out.vramEstimateMb = vram;
        var timeout = int('timeoutSec');
        if (timeout !== null && timeout > 0) out.timeoutSec = timeout;
        var idle = int('idleUnloadMinutes');
        if (idle !== null && idle > 0) out.idleUnloadMinutes = idle;
        out.disabled = !!form.disabled;
        out.notes = String(form.notes || '').trim();

        return out;
    }

    /** Текст textarea -> argv-массив (по строке на аргумент, обрезка пробелов). */
    function splitArgsText(text) {
        return String(text === null || text === undefined ? '' : text)
            .split(/\r?\n/)
            .map(function (s) { return s.trim(); })
            .filter(function (s) { return s !== ''; });
    }

    /**
     * Валидация ДО отправки - зеркало types.ValidateImageModelProfile
     * (pkg/types/image_model.go:225-317) плюс проверка кратности 64, чтобы не
     * гонять заведомо 400-й запрос.
     *
     * Возвращает {ok, errors:[{code, ...params}]}: коды (а не текст), чтобы
     * pure-слой не зависел от языка. Тексты - в image.profiles.* (i18n).
     */
    function validateForm(form) {
        form = form || {};
        var errors = [];
        var name = String(form.name || '').trim();
        if (!name) errors.push({ code: 'name_required' });

        var family = String(form.family || '');
        if (!isValidFamily(family)) errors.push({ code: 'bad_family', family: family });

        var files = form.files || [];
        var roles = {};
        var diffusionCount = 0;
        files.forEach(function (f, idx) {
            f = f || {};
            var role = String(f.role || '').trim();
            var repo = String(f.repo || '').trim();
            var filename = String(f.filename || '').trim();
            var row = idx + 1;
            if (!role && !repo && !filename) return; // пустая строка - не ошибка
            if (!isValidRole(role)) {
                errors.push({ code: 'file_role_unknown', row: row, role: role });
                return;
            }
            if (!repo || !filename) {
                errors.push({ code: 'file_missing_fields', row: row, role: role });
                return;
            }
            if (roles[role] && role !== 'lora') {
                errors.push({ code: 'file_dup_role', row: row, role: role });
                return;
            }
            roles[role] = true;
            if (role === 'diffusion') diffusionCount++;
        });
        if (!diffusionCount) errors.push({ code: 'need_diffusion' });
        if (isDitFamily(family) && !roles.vae) errors.push({ code: 'dit_need_vae', family: family });

        var steps = intOrNull(form.defaultsSteps);
        if (steps === null || steps < LIMITS.minSteps || steps > LIMITS.maxSteps) {
            errors.push({ code: 'bad_steps', min: LIMITS.minSteps, max: LIMITS.maxSteps });
        }
        var cfg = floatOrNull(form.defaultsCfgScale);
        if (cfg === null || cfg < LIMITS.minCfg || cfg > LIMITS.maxCfg) {
            errors.push({ code: 'bad_cfg', min: LIMITS.minCfg, max: LIMITS.maxCfg });
        }
        ['Width', 'Height'].forEach(function (side) {
            var v = intOrNull(form['defaults' + side]);
            var key = side.toLowerCase();
            if (v === null || v < LIMITS.minSide || v > LIMITS.maxSide) {
                errors.push({ code: 'bad_side', side: key, min: LIMITS.minSide, max: LIMITS.maxSide });
                return;
            }
            if (v % LIMITS.sideStep !== 0) {
                errors.push({ code: 'bad_side_multiple', side: key, step: LIMITS.sideStep, value: v });
            }
        });
        var batch = intOrNull(form.defaultsBatchCount);
        if (batch === null || batch < LIMITS.minBatch || batch > LIMITS.maxBatch) {
            errors.push({ code: 'bad_batch', min: LIMITS.minBatch, max: LIMITS.maxBatch });
        }
        var seed = intOrNull(form.defaultsSeed);
        if (seed === null) seed = -1;
        if (seed < LIMITS.minSeed) errors.push({ code: 'bad_seed', min: LIMITS.minSeed });

        var layers = intOrNull(form.runtimeNGpuLayers);
        if (layers !== null && (layers < LIMITS.minGpuLayers || layers > LIMITS.maxGpuLayers)) {
            errors.push({ code: 'bad_gpu_layers', min: LIMITS.minGpuLayers, max: LIMITS.maxGpuLayers });
        }
        var autoFit = String(form.runtimeAutoFit || '');
        if (autoFit !== '' && autoFit !== 'on' && autoFit !== 'off') {
            errors.push({ code: 'bad_auto_fit' });
        }
        var splitMode = String(form.runtimeSplitMode || '');
        if (splitMode !== '' && splitMode !== 'layer' && splitMode !== 'row') {
            errors.push({ code: 'bad_split_mode' });
        }
        var tileSize = intOrNull(form.runtimeVaeTileSize);
        if (tileSize !== null && tileSize < 0) errors.push({ code: 'bad_vae_tile_size' });
        var seedMode = String(form.runtimeSeedMode || '');
        if (seedMode !== '' && seedMode !== 'random' && seedMode !== 'fixed') {
            errors.push({ code: 'bad_seed_mode' });
        }
        var threads = intOrNull(form.runtimeThreads);
        if (threads !== null && threads < 0) errors.push({ code: 'bad_threads' });
        if (form.runtimeOffloadToCpu && String(form.runtimeParamsBackend || '').trim() !== '') {
            errors.push({ code: 'offload_params_conflict' });
        }
        var vram = intOrNull(form.vramEstimateMb);
        if (vram !== null && vram < 0) errors.push({ code: 'bad_vram_estimate' });
        var timeout = intOrNull(form.timeoutSec);
        if (timeout !== null && timeout < 0) errors.push({ code: 'bad_timeout' });
        var idle = intOrNull(form.idleUnloadMinutes);
        if (idle !== null && idle < 0) errors.push({ code: 'bad_idle_unload' });

        return { ok: errors.length === 0, errors: errors };
    }

    function intOrNull(value) {
        if (value === null || value === undefined) return null;
        var s = String(value).trim();
        if (s === '') return null;
        if (!/^[+-]?\d+$/.test(s)) return null;
        var n = parseInt(s, 10);
        return isNaN(n) ? null : n;
    }

    function floatOrNull(value) {
        if (value === null || value === undefined) return null;
        var s = String(value).trim();
        if (s === '') return null;
        var n = parseFloat(s);
        return isNaN(n) ? null : n;
    }

    /**
     * Разбор ответа apply/apply-progress.
     * Формы: {model,profile,applyId,status,backends:[{backendId,status,message}]},
     * обёртка {progress:{...}} или {data:{...}}, голый список бэкендов.
     *
     * Статусы бэкенда (handlers_image_profiles.go:139-143):
     *   persisted            - профиль сохранён, воркер доступен;
     *   unreachable          - воркер не ответил (профиль сохранён, но НЕ применён);
     *   not_an_image_backend - бэкенд не image-типа;
     *   error                - HTTP-ошибка воркера.
     */
    function normalizeApplyResponse(data) {
        var src = data || {};
        if (src.progress && typeof src.progress === 'object') src = src.progress;
        else if (src.data && typeof src.data === 'object' && !Array.isArray(src.data)) src = src.data;
        var backends = [];
        if (Array.isArray(src)) backends = src;
        else if (Array.isArray(src.backends)) backends = src.backends;
        var out = {
            model: String(src.model || src.name || ''),
            applyId: String(src.applyId || src.apply_id || src.id || ''),
            status: String(src.status || '').toLowerCase(),
            backends: backends.map(function (b) {
                b = b || {};
                return {
                    backendId: String(b.backendId || b.backend_id || b.id || b.name || ''),
                    status: String(b.status || '').toLowerCase(),
                    message: String(b.message || b.error || '')
                };
            }),
            error: String(src.error || src.message || '')
        };
        out.total = out.backends.length;
        out.persisted = out.backends.filter(function (b) { return b.status === 'persisted'; }).length;
        out.unreachable = out.backends.filter(function (b) { return b.status === 'unreachable'; }).length;
        out.failed = out.backends.filter(function (b) { return b.status === 'error'; }).length;
        out.running = out.status === 'running' || out.status === 'pending' || out.status === 'accepted';
        out.done = !!(out.status && !out.running);
        if (!out.status) out.done = out.backends.length > 0;
        // Итог: applied (есть хотя бы один persisted и нет unreachable/error),
        // partial (часть не применилась), unreachable (ни один воркер не ответил).
        if (out.total === 0) out.outcome = out.done ? 'unknown' : 'running';
        else if (out.unreachable > 0 && out.persisted === 0 && out.failed === 0) out.outcome = 'unreachable';
        else if (out.failed > 0 || out.unreachable > 0) out.outcome = 'partial';
        else if (out.persisted > 0) out.outcome = 'applied';
        else out.outcome = 'unknown';
        return out;
    }

    /** Список профилей -> массив [{name, profile}] по алфавиту. */
    function normalizeProfileList(data) {
        var models = {};
        if (data && data.models && typeof data.models === 'object' && !Array.isArray(data.models)) {
            models = data.models;
        } else if (Array.isArray(data)) {
            data.forEach(function (p) {
                if (p && p.name) models[p.name] = p;
            });
        } else if (data && Array.isArray(data.profiles)) {
            data.profiles.forEach(function (p) {
                if (p && p.name) models[p.name] = p;
            });
        }
        return Object.keys(models).sort().map(function (name) {
            var p = models[name] || {};
            if (!p.name) p.name = name;
            return { name: name, profile: p };
        });
    }

    /** Каталог -> массив пресетов [{name, profile}] (их подставляем как основу). */
    function normalizeCatalog(data) {
        var presets = [];
        if (data && Array.isArray(data.presets)) presets = data.presets;
        else if (Array.isArray(data)) presets = data;
        return presets.filter(function (p) { return p && (p.name || p.family); }).map(function (p) {
            return { name: String(p.name || p.family || ''), profile: p };
        });
    }

    /**
     * Бэкенды -> [{id,name,host,imagePort,status}].
     * @param {*} data ответ /api/v1/image/backends или /api/v1/backends
     * @param {boolean} onlyImageType true для /api/v1/backends (там все типы).
     */
    function normalizeBackends(data, onlyImageType) {
        var arr = [];
        if (Array.isArray(data)) arr = data;
        else if (data && Array.isArray(data.backends)) arr = data.backends;
        return arr.map(function (b) {
            b = b || {};
            var type = String(b.type || b.backendType || b.backend_type || b.engine || '').toLowerCase();
            return {
                id: b.id || b.ID || '',
                name: b.name || b.id || '',
                host: b.host || '',
                imagePort: Number(b.imagePort || b.image_port || 0),
                status: String(b.status || ''),
                type: type
            };
        }).filter(function (b) {
            if (!b.id) return false;
            if (!onlyImageType) return true;
            return b.type === 'image_cpp' || b.type === 'sd_cpp' || b.type === 'sdcpp';
        });
    }

    /** Текст ошибки из любого конверта ответа (OpenAI/плоский/сырой). */
    function parseApiError(body, status) {
        var fallback = 'HTTP ' + (status || '?');
        if (body === null || body === undefined || body === '') return fallback;
        if (typeof body === 'string') {
            var text = body.trim();
            if (!text) return fallback;
            if (text.charAt(0) === '{' || text.charAt(0) === '[') {
                try { return parseApiError(JSON.parse(text), status); } catch (e) { /* не JSON */ }
            }
            return text.length > 400 ? text.slice(0, 400) + '...' : text;
        }
        if (typeof body === 'object') {
            var err = body.error;
            if (typeof err === 'string' && err) {
                if (body.message && body.message !== err) return err + ': ' + String(body.message);
                return err;
            }
            if (err && typeof err === 'object') {
                var msg = err.message || err.code || '';
                if (msg) return String(msg);
            }
            if (body.message) return String(body.message);
            if (body.detail) return String(body.detail);
        }
        return fallback;
    }

    var pure = {
        escapeHtml: escapeHtml,
        isDitFamily: isDitFamily,
        isValidFamily: isValidFamily,
        isValidRole: isValidRole,
        formatBytes: formatBytes,
        formatDuration: formatDuration,
        clampInt: clampInt,
        bundleSizeBytes: bundleSizeBytes,
        familyDefaultsFor: familyDefaultsFor,
        profileToForm: profileToForm,
        formToProfile: formToProfile,
        filesFromForm: filesFromForm,
        splitArgsText: splitArgsText,
        validateForm: validateForm,
        normalizeApplyResponse: normalizeApplyResponse,
        normalizeProfileList: normalizeProfileList,
        normalizeCatalog: normalizeCatalog,
        normalizeBackends: normalizeBackends,
        parseApiError: parseApiError,
        intOrNull: intOrNull,
        floatOrNull: floatOrNull,
        IMAGE_ROLES: IMAGE_ROLES,
        IMAGE_FAMILIES: IMAGE_FAMILIES,
        DIT_FAMILIES: DIT_FAMILIES,
        ROLE_ORDER: ROLE_ORDER,
        SAMPLER_SUGGESTIONS: SAMPLER_SUGGESTIONS,
        SCHEDULER_SUGGESTIONS: SCHEDULER_SUGGESTIONS,
        LIMITS: LIMITS,
        APPLY_POLL_MS: APPLY_POLL_MS,
        APPLY_POLL_MAX: APPLY_POLL_MAX
    };

    // =====================================================================
    // i18n / toast / сеть
    // =====================================================================

    function t(key, fallback, vars) {
        if (window.I18N && window.I18N.t) {
            var s = window.I18N.t(key, vars);
            if (s !== key) return s;
        }
        var base = fallback !== undefined ? fallback : key;
        if (vars) {
            Object.keys(vars).forEach(function (k) {
                base = String(base).replace('{' + k + '}', vars[k]);
            });
        }
        return base;
    }

    function toast(msg, type) {
        if (typeof window.showToast === 'function') { window.showToast(msg, type); return; }
        if (window.App && typeof window.App.showToast === 'function') { window.App.showToast(msg, type); return; }
        if (window.console) console.warn('[ImageProfiles]', msg);
    }

    function apiBase() {
        var cfg = window.WEBUI_CONFIG || {};
        var base = cfg.API_BASE_URL || cfg.API_BASE ||
            (typeof window !== 'undefined' && window.location && window.location.origin) || '';
        return String(base).replace(/\/+$/, '');
    }

    function apiToken() {
        var cfg = window.WEBUI_CONFIG || {};
        if (cfg.API_TOKEN) return cfg.API_TOKEN;
        try { return localStorage.getItem('apiToken') || ''; } catch (e) { return ''; }
    }

    /** Базовые заголовки: общий хелпер проекта + fallback токена из localStorage. */
    function authHeaders(extra) {
        var headers = (window.Api && typeof window.Api.getAuthHeaders === 'function')
            ? window.Api.getAuthHeaders()
            : { 'Content-Type': 'application/json' };
        if (extra) {
            Object.keys(extra).forEach(function (k) { headers[k] = extra[k]; });
        }
        if (!headers['X-API-Token']) {
            var tk = apiToken();
            if (tk) headers['X-API-Token'] = tk;
        }
        return headers;
    }

    function requestJson(path, opts) {
        opts = opts || {};
        var timeoutMs = opts.timeoutMs || TIMEOUT_CONTROL_MS;
        var ctrl = (typeof AbortController !== 'undefined') ? new AbortController() : null;
        var timer = null;
        if (ctrl) timer = setTimeout(function () { ctrl.abort(); }, timeoutMs);
        var init = { method: opts.method || 'GET', headers: authHeaders(opts.headers) };
        if (ctrl) init.signal = ctrl.signal;
        if (opts.body !== undefined) init.body = opts.body;
        return fetch(apiBase() + path, init).then(function (resp) {
            if (timer) clearTimeout(timer);
            return resp.text().then(function (text) {
                var parsed = null;
                if (text) {
                    try { parsed = JSON.parse(text); } catch (e) { parsed = text; }
                }
                if (!resp.ok) {
                    var err = new Error(parseApiError(parsed, resp.status));
                    err.status = resp.status;
                    err.body = parsed;
                    throw err;
                }
                return parsed;
            });
        }).catch(function (err) {
            if (timer) clearTimeout(timer);
            if (err && err.name === 'AbortError') {
                var e2 = new Error('timeout after ' + Math.round(timeoutMs / 1000) + 's');
                e2.code = 'timeout';
                throw e2;
            }
            throw err;
        });
    }

    function isMissingEndpoint(err) {
        return !!err && (err.status === 404 || err.status === 405 || err.status === 501);
    }

    /** API профилей (отдельный namespace: image-page.js его не знает). */
    var api = {
        list: function () { return requestJson(PATH_PROFILES); },
        get: function (name) { return requestJson(PATH_PROFILES + '/' + encodeURIComponent(name)); },
        put: function (name, body) {
            return requestJson(PATH_PROFILES + '/' + encodeURIComponent(name), {
                method: 'PUT', body: JSON.stringify(body)
            });
        },
        remove: function (name) {
            return requestJson(PATH_PROFILES + '/' + encodeURIComponent(name), { method: 'DELETE' });
        },
        apply: function (name, body) {
            return requestJson(PATH_PROFILES + '/' + encodeURIComponent(name) + '/apply', {
                method: 'POST', body: body ? JSON.stringify(body) : undefined
            });
        },
        applyProgress: function (name, applyId) {
            var qs = applyId ? ('?applyId=' + encodeURIComponent(applyId)) : '';
            return requestJson(PATH_PROFILES + '/' + encodeURIComponent(name) + '/apply/progress' + qs);
        },
        applyStatus: function (name, applyId) {
            return requestJson(PATH_PROFILES + '/' + encodeURIComponent(name) + '/apply/status/' + encodeURIComponent(applyId));
        },
        catalog: function () { return requestJson(PATH_CATALOG); },
        backends: async function () {
            try {
                return normalizeBackends(await requestJson(PATH_BACKENDS), false);
            } catch (e) {
                // /api/v1/image/backends - часть параллельной бэкенд-работы: если его
                // ещё нет, берём общий список и фильтруем по типу сами.
                if (isMissingEndpoint(e) || e.code === 'timeout') {
                    return normalizeBackends(await requestJson(PATH_BACKENDS_FALLBACK), true);
                }
                throw e;
            }
        }
    };

    // =====================================================================
    // Состояние
    // =====================================================================

    var state = {
        mounted: false,
        bound: false,
        loaded: false,
        container: null,
        profiles: [],
        catalog: [],
        backends: [],
        // Локальный статус применения: {name: {state, applyId, outcome, error, at}}
        applied: {},
        selected: '',
        busy: false,
        applyPoll: null,
        loadError: ''
    };

    // =====================================================================
    // Рендер: список профилей
    // =====================================================================

    function el(id) {
        return document.getElementById(id);
    }

    function renderProfiles() {
        var body = el('imageProfilesBody');
        if (!body) return;
        var count = el('imageProfilesCount');
        if (count) {
            count.textContent = state.loaded
                ? t('image.profiles.count', 'Profiles: {n}', { n: state.profiles.length })
                : '';
        }
        if (!state.loaded) {
            body.innerHTML = '<tr><td colspan="6" class="loading-cell">' +
                escapeHtml(t('common.loading', 'Loading...')) + '</td></tr>';
            return;
        }
        if (!state.profiles.length) {
            body.innerHTML = '<tr><td colspan="6" class="loading-cell">' +
                escapeHtml(t('image.profiles.empty', 'No image model profiles yet. Create one with "New profile".')) +
                '</td></tr>';
            return;
        }
        body.innerHTML = state.profiles.map(function (entry) {
            var p = entry.profile || {};
            var name = entry.name;
            var size = bundleSizeBytes(p.files);
            var apply = state.applied[name] || null;
            var statusHtml = '';
            if (apply) {
                var cls = 'is-none';
                if (apply.outcome === 'applied') cls = 'is-default';
                else if (apply.outcome === 'partial') cls = 'is-env';
                else if (apply.outcome === 'unreachable') cls = 'is-env';
                statusHtml = '<span class="cpp-source-badge ' + cls + '">' +
                    escapeHtml(t('image.profiles.state_' + (apply.outcome || 'unknown'), apply.outcome || 'unknown')) +
                    '</span>';
                if (apply.unreachable) {
                    statusHtml += ' <span class="imgp-unreachable" title="' +
                        escapeHtml(t('image.profiles.unreachable_hint', 'The profile is saved on the balancer, but the image worker did not answer, so the running sd-server did not get it.')) +
                        '">' + escapeHtml(t('image.profiles.unreachable', 'worker unreachable')) + '</span>';
                }
            } else {
                statusHtml = '<span class="cpp-source-badge is-none">' +
                    escapeHtml(t('image.profiles.not_applied', 'not applied')) + '</span>';
            }
            var fam = p.family || '';
            var editLabel = t('common.edit', 'Edit');
            return '<tr>' +
                '<td><strong>' + escapeHtml(name) + '</strong>' +
                    (p.disabled ? ' <span class="badge">' + escapeHtml(t('image.profiles.disabled_badge', 'disabled')) + '</span>' : '') +
                    (p.notes ? '<div class="imgp-notes">' + escapeHtml(p.notes) + '</div>' : '') +
                '</td>' +
                '<td>' + escapeHtml(fam || '-') + '</td>' +
                '<td>' + (size > 0 ? escapeHtml(formatBytes(size)) : '-') + '</td>' +
                '<td>' + (p.vramEstimateMb ? escapeHtml(String(p.vramEstimateMb)) + ' MB' : '-') + '</td>' +
                '<td>' + statusHtml + '</td>' +
                '<td><div class="imgp-actions">' +
                    '<button class="btn btn-secondary btn-sm" data-imgp-action="edit" data-name="' + escapeHtml(name) + '">' + escapeHtml(editLabel) + '</button>' +
                    '<button class="btn btn-secondary btn-sm" data-imgp-action="duplicate" data-name="' + escapeHtml(name) + '">' + escapeHtml(t('image.profiles.duplicate', 'Duplicate')) + '</button>' +
                    '<button class="btn btn-primary btn-sm" data-imgp-action="apply" data-name="' + escapeHtml(name) + '">' + escapeHtml(t('image.profiles.apply', 'Apply')) + '</button>' +
                    '<button class="btn btn-danger btn-sm" data-imgp-action="delete" data-name="' + escapeHtml(name) + '">' + escapeHtml(t('common.delete', 'Delete')) + '</button>' +
                '</div></td>' +
                '</tr>';
        }).join('');
    }

    function renderNotice() {
        var box = el('imageProfilesNotice');
        if (!box) return;
        if (!state.loadError) { box.style.display = 'none'; box.innerHTML = ''; return; }
        box.innerHTML = '<span class="imgp-error">' + escapeHtml(state.loadError) + '</span>';
        box.style.display = '';
    }

    // =====================================================================
    // Загрузка данных
    // =====================================================================

    async function loadProfiles() {
        try {
            var data = await api.list();
            state.profiles = normalizeProfileList(data);
            state.loadError = '';
            state.loaded = true;
        } catch (e) {
            state.profiles = [];
            state.loaded = true;
            state.loadError = t('image.profiles.load_failed', 'Failed to load image model profiles: {error}',
                { error: (e && e.message) || String(e) });
        }
        renderNotice();
        renderProfiles();
        return state.profiles;
    }

    async function loadCatalog() {
        try {
            state.catalog = normalizeCatalog(await api.catalog());
        } catch (e) {
            // Каталог - необязательная подсказка: без него профиль всё равно
            // можно заполнить руками, поэтому не засоряем экран ошибкой.
            state.catalog = [];
        }
        return state.catalog;
    }

    /** Полный refresh данных (монтирование, кнопка «Обновить», хук image-page). */
    async function refresh() {
        await Promise.all([loadProfiles(), loadCatalog()]);
        return state.profiles;
    }

    // =====================================================================
    // Редактор профиля (modal, образец - cppworker-params.js openWizard)
    // =====================================================================

    /** Закрыть редактор: чистим только контейнер index.html, хост-модалку не удаляем. */
    function closeEditor() {
        var host = document.getElementById('imageProfileEditor');
        if (host) host.innerHTML = '';
    }

    function roleOptions(selected) {
        return IMAGE_ROLES.map(function (role) {
            return '<option value="' + role + '"' + (role === selected ? ' selected' : '') + '>' + role + '</option>';
        }).join('');
    }

    function fileRowHtml(row, index) {
        row = row || {};
        var role = String(row.role || 'diffusion');
        var localPath = String(row.localPath || '');
        var size = Number(row.sizeBytes || 0);
        return '<div class="imgp-file-row" data-imgp-file-row="' + index + '">' +
            '<div class="imgp-file-col imgp-file-col-role">' +
                '<label>' + escapeHtml(t('image.profiles.file_role', 'Role')) + '</label>' +
                '<select class="form-control" data-imgp-file="role">' + roleOptions(role) + '</select>' +
            '</div>' +
            '<div class="imgp-file-col imgp-file-col-repo">' +
                '<label>' + escapeHtml(t('image.profiles.file_repo', 'Repo (org/model)')) + '</label>' +
                '<input type="text" class="form-control" data-imgp-file="repo" value="' + escapeHtml(row.repo || '') + '" placeholder="leejet/Z-Image-Turbo-GGUF">' +
            '</div>' +
            '<div class="imgp-file-col imgp-file-col-file">' +
                '<label>' + escapeHtml(t('image.profiles.file_filename', 'File in repo')) + '</label>' +
                '<input type="text" class="form-control" data-imgp-file="filename" value="' + escapeHtml(row.filename || '') + '" placeholder="z-image-turbo-Q3_K.gguf">' +
            '</div>' +
            '<div class="imgp-file-col imgp-file-col-rev">' +
                '<label>' + escapeHtml(t('image.profiles.file_revision', 'Revision')) + '</label>' +
                '<input type="text" class="form-control" data-imgp-file="revision" value="' + escapeHtml(row.revision || 'main') + '" placeholder="main">' +
            '</div>' +
            '<div class="imgp-file-col imgp-file-col-act">' +
                '<button type="button" class="btn btn-secondary btn-sm" data-imgp-action="file-remove">' + escapeHtml(t('image.profiles.file_remove', 'Remove')) + '</button>' +
            '</div>' +
            (localPath || size > 0
                ? '<div class="imgp-file-local">' + escapeHtml(t('image.profiles.file_local', 'local:')) + ' ' +
                    escapeHtml(localPath || '-') + (size > 0 ? ' (' + escapeHtml(formatBytes(size)) + ')' : '') + '</div>'
                : '') +
            '</div>';
    }

    function filesHtml(rows) {
        var list = (rows && rows.length) ? rows : [{ role: 'diffusion', repo: '', filename: '', revision: 'main' }];
        return list.map(function (row, i) { return fileRowHtml(row, i); }).join('');
    }

    function catalogOptionsHtml() {
        if (!state.catalog.length) {
            return '<option value="">' + escapeHtml(t('image.profiles.preset_none', 'No catalog presets available')) + '</option>';
        }
        return '<option value="">' + escapeHtml(t('image.profiles.preset_select', 'Select a preset...')) + '</option>' +
            state.catalog.map(function (c) {
                return '<option value="' + escapeHtml(c.name) + '">' + escapeHtml(c.name) +
                    (c.profile && c.profile.family ? ' (' + escapeHtml(c.profile.family) + ')' : '') + '</option>';
            }).join('');
    }

    function editorHtml(form, isNew) {
        var d = form;
        var numField = function (id, value) {
            return '<input type="number" class="form-control" id="' + id + '" data-imgp-field="' + id + '" value="' + escapeHtml(value) + '">';
        };
        return '' +
        '<div class="mode-wizard-overlay imgp-editor" id="imgpEditorOverlay">' +
          '<div class="mode-wizard-modal imgp-editor-modal">' +
            '<div class="wizard-header">' +
              '<h3>' + escapeHtml(isNew ? t('image.profiles.new_title', 'New image model profile') : t('image.profiles.edit_title', 'Edit image model profile')) + '</h3>' +
            '</div>' +
            '<div class="wizard-desc">' +
              escapeHtml(t('image.profiles.editor_hint', 'A profile is a bundle of files (diffusion + VAE + text encoders) plus generation defaults and sd-server runtime flags. Saving only stores the profile: the running worker picks it up on the next model load, or right away via Apply.')) +
            '</div>' +
            '<div class="imgp-errors" id="imgpErrors" style="display:none;"></div>' +
            '<div class="wizard-fields">' +

              // ---- basic ----
              '<details open><summary class="imgp-section-title">' + escapeHtml(t('image.profiles.section_basic', 'Basic')) + '</summary>' +
              '<div class="imgp-grid">' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.name', 'Name')) + ' *</span>' +
                  '<input type="text" class="form-control" id="imgpName" data-imgp-field="imgpName" value="' + escapeHtml(d.name) + '"' + (isNew ? '' : ' readonly') + ' placeholder="z-image-turbo-q3k"></label>' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.family', 'Family')) + '</span>' +
                  '<select class="form-control" id="imgpFamily" data-imgp-field="imgpFamily">' +
                    IMAGE_FAMILIES.map(function (f) {
                        return '<option value="' + f + '"' + (f === d.family ? ' selected' : '') + '>' + f + '</option>';
                    }).join('') +
                  '</select></label>' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.vram', 'VRAM estimate, MB')) + '</span>' +
                  numField('imgpVram', d.vramEstimateMb) + '</label>' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.timeout', 'Timeout, sec (0 = family default)')) + '</span>' +
                  numField('imgpTimeout', d.timeoutSec) + '</label>' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.idle_unload', 'Idle unload, minutes (0 = never)')) + '</span>' +
                  numField('imgpIdleUnload', d.idleUnloadMinutes) + '</label>' +
                '<label class="imgp-field imgp-field-check"><input type="checkbox" id="imgpDisabled" data-imgp-field="imgpDisabled"' + (d.disabled ? ' checked' : '') + '>' +
                  '<span>' + escapeHtml(t('image.profiles.disabled', 'Disabled (do not use for new requests)')) + '</span></label>' +
              '</div>' +
              '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.notes', 'Notes')) + '</span>' +
                '<textarea class="form-control" rows="2" id="imgpNotes" data-imgp-field="imgpNotes">' + escapeHtml(d.notes) + '</textarea></label>' +
              '</details>' +

              // ---- files ----
              '<details open><summary class="imgp-section-title">' + escapeHtml(t('image.profiles.section_files', 'Bundle files')) + '</summary>' +
              '<div class="imgp-catalog-row">' +
                '<select class="form-control" id="imgpPreset">' + catalogOptionsHtml() + '</select>' +
                '<button type="button" class="btn btn-secondary" data-imgp-action="fill-from-catalog" id="imgpFillPreset">' +
                  escapeHtml(t('image.profiles.fill_from_catalog', 'Fill from catalog')) + '</button>' +
              '</div>' +
              '<small class="imgp-hint">' + escapeHtml(t('image.profiles.catalog_hint', 'A catalog preset replaces all file rows and generation defaults. Then edit what you need.')) + '</small>' +
              '<div id="imgpFileRows">' + filesHtml(d.files) + '</div>' +
              '<button type="button" class="btn btn-secondary" data-imgp-action="file-add" id="imgpFileAdd">' +
                escapeHtml(t('image.profiles.file_add', 'Add file')) + '</button>' +
              '</details>' +

              // ---- defaults ----
              '<details open><summary class="imgp-section-title">' + escapeHtml(t('image.profiles.section_defaults', 'Generation defaults')) + '</summary>' +
              '<div class="imgp-grid">' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.steps', 'Steps (1..100)')) + '</span>' + numField('imgpSteps', d.defaultsSteps) + '</label>' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.cfg', 'CFG scale (0..30)')) + '</span>' + numField('imgpCfg', d.defaultsCfgScale) + '</label>' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.width', 'Width (64..4096, step 64)')) + '</span><input type="number" class="form-control" step="64" id="imgpWidth" data-imgp-field="imgpWidth" value="' + escapeHtml(d.defaultsWidth) + '"></label>' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.height', 'Height (64..4096, step 64)')) + '</span><input type="number" class="form-control" step="64" id="imgpHeight" data-imgp-field="imgpHeight" value="' + escapeHtml(d.defaultsHeight) + '"></label>' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.batch', 'Batch count (1..8)')) + '</span>' + numField('imgpBatch', d.defaultsBatchCount) + '</label>' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.seed', 'Seed (-1 = random)')) + '</span>' + numField('imgpSeed', d.defaultsSeed) + '</label>' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.clip_skip', 'CLIP skip (0/1 = do not pass)')) + '</span>' + numField('imgpClipSkip', d.defaultsClipSkip) + '</label>' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.sampler', 'Sampler')) + '</span>' +
                  '<input type="text" class="form-control" id="imgpSampler" data-imgp-field="imgpSampler" value="' + escapeHtml(d.defaultsSampler) + '" list="imgpSamplerList">' +
                  '<datalist id="imgpSamplerList">' + SAMPLER_SUGGESTIONS.map(function (s) { return '<option value="' + escapeHtml(s) + '"></option>'; }).join('') + '</datalist></label>' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.scheduler', 'Scheduler')) + '</span>' +
                  '<input type="text" class="form-control" id="imgpScheduler" data-imgp-field="imgpScheduler" value="' + escapeHtml(d.defaultsScheduler) + '" list="imgpSchedulerList">' +
                  '<datalist id="imgpSchedulerList">' + SCHEDULER_SUGGESTIONS.map(function (s) { return '<option value="' + escapeHtml(s) + '"></option>'; }).join('') + '</datalist></label>' +
              '</div>' +
              '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.negative', 'Negative prompt')) + '</span>' +
                '<textarea class="form-control" rows="2" id="imgpNegative" data-imgp-field="imgpNegative">' + escapeHtml(d.defaultsNegativePrompt) + '</textarea></label>' +
              '<small class="imgp-hint">' + escapeHtml(t('image.profiles.merge_hint', 'The server merges updates: a zero or an empty value means "keep the current one", so fields cannot be reset to zero from here - put the value you need or re-create the profile.')) + '</small>' +
              '</details>' +

              // ---- runtime ----
              '<details><summary class="imgp-section-title">' + escapeHtml(t('image.profiles.section_runtime', 'Runtime (sd-server flags)')) + '</summary>' +
              '<div class="imgp-grid">' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.backend_flag', '--backend (te=cpu,vae=cuda0,...)')) + '</span>' +
                  '<input type="text" class="form-control" id="imgpRuntimeBackend" data-imgp-field="imgpRuntimeBackend" value="' + escapeHtml(d.runtimeBackend) + '" placeholder="te=cpu,vae=cuda0"></label>' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.params_backend', '--params-backend (cpu | disk | diffusion=disk)')) + '</span>' +
                  '<select class="form-control" id="imgpParamsBackend" data-imgp-field="imgpParamsBackend">' +
                    ['', 'cpu', 'disk', 'diffusion=disk'].map(function (v) {
                        return '<option value="' + escapeHtml(v) + '"' + (v === d.runtimeParamsBackend ? ' selected' : '') + '>' + (v === '' ? '-' : escapeHtml(v)) + '</option>';
                    }).join('') +
                  '</select></label>' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.max_vram', '--max-vram (6 | -1 | cuda0=6)')) + '</span>' +
                  '<input type="text" class="form-control" id="imgpMaxVram" data-imgp-field="imgpMaxVram" value="' + escapeHtml(d.runtimeMaxVram) + '" placeholder="6"></label>' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.auto_fit', '--auto-fit (on|off)')) + '</span>' +
                  '<select class="form-control" id="imgpAutoFit" data-imgp-field="imgpAutoFit">' +
                    ['', 'on', 'off'].map(function (v) {
                        return '<option value="' + v + '"' + (v === d.runtimeAutoFit ? ' selected' : '') + '>' + (v === '' ? '-' : v) + '</option>';
                    }).join('') +
                  '</select></label>' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.split_mode', '--split-mode (layer|row)')) + '</span>' +
                  '<select class="form-control" id="imgpSplitMode" data-imgp-field="imgpSplitMode">' +
                    ['', 'layer', 'row'].map(function (v) {
                        return '<option value="' + v + '"' + (v === d.runtimeSplitMode ? ' selected' : '') + '>' + (v === '' ? '-' : v) + '</option>';
                    }).join('') +
                  '</select></label>' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.gpu_layers', '--n-gpu-layers (-1..999)')) + '</span>' + numField('imgpGpuLayers', d.runtimeNGpuLayers) + '</label>' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.threads', '--threads (0 = engine default)')) + '</span>' + numField('imgpThreads', d.runtimeThreads) + '</label>' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.vae_tile_size', '--vae-tile-size, image pixels (0 = engine default)')) + '</span>' + numField('imgpVaeTileSize', d.runtimeVaeTileSize) + '</label>' +
                '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.seed_mode', 'Seed mode')) + '</span>' +
                  '<select class="form-control" id="imgpSeedMode" data-imgp-field="imgpSeedMode">' +
                    ['random', 'fixed'].map(function (v) {
                        return '<option value="' + v + '"' + (v === d.runtimeSeedMode ? ' selected' : '') + '>' + v + '</option>';
                    }).join('') +
                  '</select></label>' +
              '</div>' +
              '<div class="imgp-checks">' +
                '<label class="imgp-field imgp-field-check" title="' + escapeHtml(t('image.profiles.offload_tip', 'Shortcut for --params-backend "*=cpu". Mutually exclusive with --params-backend: the explicit flag wins and disables --auto-fit.')) + '">' +
                  '<input type="checkbox" id="imgpOffloadToCpu" data-imgp-field="imgpOffloadToCpu"' + (d.runtimeOffloadToCpu ? ' checked' : '') + '>' +
                  '<span>' + escapeHtml(t('image.profiles.offload_to_cpu', 'offloadToCpu (weights to RAM)')) + '</span></label>' +
                '<label class="imgp-field imgp-field-check" title="' + escapeHtml(t('image.profiles.fa_tip', '--diffusion-fa: flash attention for the diffusion model. Faster and less VRAM on most GPUs.')) + '">' +
                  '<input type="checkbox" id="imgpDiffusionFa" data-imgp-field="imgpDiffusionFa"' + (d.runtimeDiffusionFa ? ' checked' : '') + '>' +
                  '<span>' + escapeHtml(t('image.profiles.diffusion_fa', 'diffusionFa (flash attention)')) + '</span></label>' +
                '<label class="imgp-field imgp-field-check" title="' + escapeHtml(t('image.profiles.vae_tiling_tip', '--vae-tiling: prefer this over putting the VAE on CPU (vae=cpu costs about 5x slower decode).')) + '">' +
                  '<input type="checkbox" id="imgpVaeTiling" data-imgp-field="imgpVaeTiling"' + (d.runtimeVaeTiling ? ' checked' : '') + '>' +
                  '<span>' + escapeHtml(t('image.profiles.vae_tiling', 'vaeTiling (tiled VAE decode)')) + '</span></label>' +
                '<label class="imgp-field imgp-field-check" title="' + escapeHtml(t('image.profiles.vae_conv_direct_tip', '--vae-conv-direct: lower VRAM during VAE decode on some GPUs. Try it if decode fails with OOM.')) + '">' +
                  '<input type="checkbox" class="form-check-input" id="imgpVaeConvDirect" data-imgp-field="imgpVaeConvDirect"' + (d.runtimeVaeConvDirect ? ' checked' : '') + '>' +
                  '<span>' + escapeHtml(t('image.profiles.vae_conv_direct', 'vaeConvDirect')) + '</span></label>' +
                '<label class="imgp-field imgp-field-check" title="' + escapeHtml(t('image.profiles.taesd_tip', '--taesd: tiny autoencoder for a fast preview and much less VRAM on decode. The file is taken from the worker directory unless a taesd file role is set.')) + '">' +
                  '<input type="checkbox" id="imgpTaesd" data-imgp-field="imgpTaesd"' + (d.runtimeTaesd ? ' checked' : '') + '>' +
                  '<span>' + escapeHtml(t('image.profiles.taesd', 'taesd (tiny autoencoder)')) + '</span></label>' +
              '</div>' +
              '<label class="imgp-field"><span>' + escapeHtml(t('image.profiles.extra_args', 'Extra sd-server flags, one per line')) + '</span>' +
                '<textarea class="form-control" rows="3" id="imgpExtraArgs" data-imgp-field="imgpExtraArgs" placeholder="--vae-tile-overlap 0.25">' + escapeHtml((d.runtimeExtraArgs || []).join('\n')) + '</textarea></label>' +
              '<small class="imgp-hint">' + escapeHtml(t('image.profiles.runtime_hint', 'Forcing more work onto the CPU (offload, paramsBackend) trades speed for VRAM. seedMode random resolves a fresh seed for every request; fixed reuses the Defaults seed.')) + '</small>' +
              '<div id="imgpServerArgs" class="imgp-server-args"></div>' +
              '</details>' +
            '</div>' +

            '<div class="wizard-footer">' +
              '<button type="button" class="btn btn-secondary" id="imgpCancel">' + escapeHtml(t('common.cancel', 'Cancel')) + '</button>' +
              '<button type="button" class="btn btn-secondary" id="imgpSaveApply">' + escapeHtml(t('image.profiles.save_and_apply', 'Save and apply')) + '</button>' +
              '<button type="button" class="btn btn-primary" id="imgpSave">' + escapeHtml(t('common.save', 'Save')) + '</button>' +
            '</div>' +
          '</div>' +
        '</div>';
    }

    /** Чтение формы из DOM (doc = #imgpEditorOverlay). Работает без DOM-глобалей. */
    function readFormFromDoc(doc) {
        var form = {};
        if (!doc || !doc.querySelector) return form;
        doc.querySelectorAll('[data-imgp-field]').forEach(function (node) {
            var id = node.getAttribute('data-imgp-field');
            if (node.type === 'checkbox') form[id] = !!node.checked;
            else form[id] = node.value === undefined ? '' : String(node.value);
        });
        form.imgpFileRows = readFilesFromDoc(doc);
        return form;
    }

    function readFilesFromDoc(doc) {
        var rows = [];
        if (!doc || !doc.querySelectorAll) return rows;
        doc.querySelectorAll('[data-imgp-file-row]').forEach(function (row) {
            var get = function (field) {
                var node = row.querySelector('[data-imgp-file="' + field + '"]');
                return node && node.value !== undefined ? String(node.value) : '';
            };
            rows.push({
                role: get('role'),
                repo: get('repo'),
                filename: get('filename'),
                revision: get('revision')
            });
        });
        return rows;
    }

    /**
     * Разбор DOM-формы -> состояние формы для pure-слоя.
     *
     * data-imgp-field в разметке названы по DOM-id (imgpSteps), а контракт
     * validateForm/formToProfile работает с именами формы (defaultsSteps),
     * поэтому здесь снимаем префикс imgp. DOM-id оставлены как в остальном
     * WebUI (префикс модуля), а не переименованы под форму: id видны в
     * index.html и в тестах DOM-id.
     */
    function formStateFromDom(doc) {
        var raw = readFormFromDoc(doc);
        var g = function (id) { return raw['imgp' + id] === undefined ? '' : raw['imgp' + id]; };
        return {
            name: String(g('Name') || ''),
            family: String(g('Family') || ''),
            notes: String(g('Notes') || ''),
            disabled: !!raw.imgpDisabled,
            vramEstimateMb: g('Vram'),
            timeoutSec: g('Timeout'),
            idleUnloadMinutes: g('IdleUnload'),

            defaultsSteps: g('Steps'),
            defaultsCfgScale: g('Cfg'),
            defaultsSampler: String(g('Sampler') || ''),
            defaultsScheduler: String(g('Scheduler') || ''),
            defaultsWidth: g('Width'),
            defaultsHeight: g('Height'),
            defaultsBatchCount: g('Batch'),
            defaultsSeed: g('Seed'),
            defaultsClipSkip: g('ClipSkip'),
            defaultsNegativePrompt: String(g('Negative') || ''),

            runtimeBackend: String(g('RuntimeBackend') || ''),
            runtimeParamsBackend: String(g('ParamsBackend') || ''),
            runtimeMaxVram: String(g('MaxVram') || ''),
            runtimeOffloadToCpu: !!raw.imgpOffloadToCpu,
            runtimeAutoFit: String(g('AutoFit') || ''),
            runtimeDiffusionFa: !!raw.imgpDiffusionFa,
            runtimeVaeTiling: !!raw.imgpVaeTiling,
            runtimeVaeTileSize: g('VaeTileSize'),
            runtimeVaeConvDirect: !!raw.imgpVaeConvDirect,
            runtimeTaesd: !!raw.imgpTaesd,
            runtimeSplitMode: String(g('SplitMode') || ''),
            runtimeNGpuLayers: g('GpuLayers'),
            runtimeThreads: g('Threads'),
            runtimeSeedMode: String(g('SeedMode') || 'random'),
            runtimeExtraArgs: splitArgsText(g('ExtraArgs')),

            files: formFilesFromRows(raw.imgpFileRows)
        };
    }

    /** Строки файлов из DOM -> files[] (локальные пути и размеры сохраняем). */
    function formFilesFromRows(rows, baseFiles) {
        var byRole = {};
        (baseFiles || []).forEach(function (f) {
            if (f && f.role) byRole[f.role] = f;
        });
        return (rows || []).map(function (r) {
            var base = byRole[r.role] || null;
            var keepLocal = !!(base && base.repo === r.repo && base.filename === r.filename);
            return {
                role: r.role,
                repo: r.repo,
                filename: r.filename,
                revision: r.revision || 'main',
                sizeBytes: keepLocal ? Number(base.sizeBytes || 0) : 0,
                localPath: keepLocal ? String(base.localPath || '') : ''
            };
        });
    }

    /** i18n-текст ошибки валидации по её коду. */
    function errorText(err) {
        if (!err) return '';
        var key = 'image.profiles.err.' + err.code;
        var fallbacks = {
            name_required: 'Name is required',
            bad_family: 'Unknown family: {family}',
            need_diffusion: 'A file with role "diffusion" is required',
            dit_need_vae: 'Family "{family}" requires a separate "vae" file',
            file_role_unknown: 'Row {row}: unknown role "{role}"',
            file_missing_fields: 'Row {row} ({role}): repo and file are required',
            file_dup_role: 'Row {row}: role "{role}" is already used (only "lora" may repeat)',
            bad_steps: 'Steps must be in [{min}..{max}]',
            bad_cfg: 'CFG scale must be in [{min}..{max}]',
            bad_side: '{side} must be in [{min}..{max}]',
            bad_side_multiple: '{side} must be a multiple of {step} (got {value})',
            bad_batch: 'Batch count must be in [{min}..{max}]',
            bad_seed: 'Seed must be >= {min}',
            bad_gpu_layers: 'nGpuLayers must be in [{min}..{max}]',
            bad_auto_fit: 'autoFit must be "on" or "off"',
            bad_split_mode: 'splitMode must be "layer" or "row"',
            bad_vae_tile_size: 'vaeTileSize must be >= 0 (image pixels)',
            bad_seed_mode: 'seedMode must be "random" or "fixed"',
            bad_threads: 'threads must be >= 0',
            offload_params_conflict: 'offloadToCpu and paramsBackend are mutually exclusive',
            bad_vram_estimate: 'VRAM estimate must be >= 0',
            bad_timeout: 'Timeout must be >= 0',
            bad_idle_unload: 'Idle unload must be >= 0'
        };
        var fallback = fallbacks[err.code] || err.code;
        var vars = {};
        Object.keys(err).forEach(function (k) { if (k !== 'code') vars[k] = err[k]; });
        return t(key, fallback, vars);
    }

    function renderEditorErrors(errors) {
        var box = document.getElementById('imgpErrors');
        if (!box) return;
        if (!errors || !errors.length) { box.style.display = 'none'; box.innerHTML = ''; return; }
        box.innerHTML = '<div class="imgp-error-title">' + escapeHtml(t('image.profiles.fix_errors', 'Fix the following before saving:')) + '</div>' +
            '<ul>' + errors.map(function (e) { return '<li>' + escapeHtml(errorText(e)) + '</li>'; }).join('') + '</ul>';
        box.style.display = '';
    }

    /** Предпросмотр argv sd-server (только чтобы оператор увидел, что уйдёт). */
    function renderServerArgs(profile) {
        var box = document.getElementById('imgpServerArgs');
        if (!box) return;
        var args = previewServerArgs(profile);
        if (!args.length) { box.innerHTML = ''; return; }
        box.innerHTML = '<div class="imgp-hint">' + escapeHtml(t('image.profiles.server_args', 'sd-server flags that this profile will produce:')) + '</div>' +
            '<pre class="imgp-args">' + escapeHtml(args.join(' ')) + '</pre>';
    }

    /**
     * Предпросмотр argv (упрощённое зеркало types.ImageModelProfile.ServerArgs).
     * Полный источник истины - сервер: GET профиля отдаёт serverArgs, и его мы
     * показываем в панели. Здесь нужен предпросмотр ДО сохранения.
     */
    function previewServerArgs(profile) {
        if (!profile) return [];
        var args = [];
        var byRole = {};
        (profile.files || []).forEach(function (f) { if (f && f.role && !byRole[f.role]) byRole[f.role] = f; });
        var roleFlags = {
            diffusion: isDitFamily(profile.family) ? '--diffusion-model' : '--model',
            vae: '--vae', clip_l: '--clip_l', clip_g: '--clip_g', t5xxl: '--t5xxl', llm: '--llm',
            clip_vision: '--clip_vision', controlnet: '--control-net', ip_adapter: '--ip-adapter',
            upscaler: '--upscale-model'
        };
        ROLE_ORDER.forEach(function (role) {
            var flag = roleFlags[role];
            var f = byRole[role];
            if (!flag || !f) return;
            args.push(flag, f.localPath || f.filename || '');
        });
        var r = profile.runtime || {};
        if (r.taesd || byRole.taesd) args.push('--taesd');
        if (r.backend) args.push('--backend', r.backend);
        if (r.paramsBackend) args.push('--params-backend', r.paramsBackend);
        else if (r.offloadToCpu) args.push('--offload-to-cpu');
        if (r.maxVram) args.push('--max-vram', r.maxVram);
        if (r.autoFit) args.push('--auto-fit', r.autoFit);
        if (r.nGpuLayers) args.push('--n-gpu-layers', String(r.nGpuLayers));
        if (r.splitMode) args.push('--split-mode', r.splitMode);
        if (r.threads > 0) args.push('--threads', String(r.threads));
        if (r.diffusionFa) args.push('--diffusion-fa');
        if (r.vaeTiling) {
            args.push('--vae-tiling');
            if (r.vaeTileSize > 0) args.push('--vae-tile-size', String(r.vaeTileSize));
        }
        if (r.vaeConvDirect) args.push('--vae-conv-direct');
        if (r.seedMode !== 'fixed') args.push('--seed', '-1');
        if (Array.isArray(r.extraArgs)) args = args.concat(r.extraArgs);
        return args;
    }

    /** Синхронизация взаимоисключающих полей: offloadToCpu <-> paramsBackend. */
    function syncRuntimeExclusives(doc) {
        if (!doc || !doc.querySelector) return;
        var offload = doc.querySelector('[data-imgp-field="imgpOffloadToCpu"]');
        var params = doc.querySelector('[data-imgp-field="imgpParamsBackend"]');
        if (!offload || !params) return;
        params.disabled = !!offload.checked;
        if (offload.checked) params.value = '';
    }

    function fillFiles(rows) {
        var box = document.getElementById('imgpFileRows');
        if (!box) return;
        box.innerHTML = filesHtml(rows);
    }

    function refreshPreviewFromDom(doc) {
        var form = formStateFromDom(doc);
        renderServerArgs(formToProfile(form));
    }

    /**
     * Открыть редактор.
     * @param {object|null} profile профиль API (null = новый)
     * @param {object} [opts] {name: имя для нового/дубликата, isNew: bool}
     */
    function openEditor(profile, opts) {
        opts = opts || {};
        closeEditor();
        var isNew = !!opts.isNew;
        var form = profileToForm(profile || {}, true);
        if (opts.name) form.name = opts.name;
        var html = editorHtml(form, isNew);
        var host = document.getElementById('imageProfileEditor');
        if (!host) {
            // Контейнер ставит index.html (секция профилей внутри #image-page).
            // Если его нет, всё равно показываем модалку: страница не должна
            // «молча ничего не делать» из-за отсутствующего контейнера.
            host = document.createElement('div');
            host.id = 'imgpEditorHost';
            document.body.appendChild(host);
        }
        host.innerHTML = html;
        var doc = document.getElementById('imgpEditorOverlay') || host;

        syncRuntimeExclusives(doc);
        refreshPreviewFromDom(doc);

        var offloadEl = doc.querySelector('[data-imgp-field="imgpOffloadToCpu"]');
        if (offloadEl) offloadEl.addEventListener('change', function () {
            syncRuntimeExclusives(doc);
            refreshPreviewFromDom(doc);
        });
        var paramsEl = doc.querySelector('[data-imgp-field="imgpParamsBackend"]');
        if (paramsEl) paramsEl.addEventListener('change', function () { refreshPreviewFromDom(doc); });

        var familyEl = doc.querySelector('[data-imgp-field="imgpFamily"]');
        if (familyEl) familyEl.addEventListener('change', function () {
            // Пустые числовые поля заполняем дефолтами нового семейства - иначе
            // оператор увидит «пусто», а сервер молча подставит свои значения.
            applyFamilyDefaultsToDoc(doc, familyEl.value);
            refreshPreviewFromDom(doc);
        });

        var rowsBox = document.getElementById('imgpFileRows');
        if (rowsBox) {
            rowsBox.addEventListener('change', function () { refreshPreviewFromDom(doc); });
            rowsBox.addEventListener('input', function () { refreshPreviewFromDom(doc); });
        }

        var fillBtn = document.getElementById('imgpFillPreset');
        if (fillBtn) fillBtn.addEventListener('click', function () { onFillFromCatalog(doc); });
        var addBtn = document.getElementById('imgpFileAdd');
        if (addBtn) addBtn.addEventListener('click', function () {
            var rows = readFilesFromDoc(doc);
            rows.push({ role: nextSuggestedRole(rows), repo: '', filename: '', revision: 'main' });
            fillFiles(rows);
            refreshPreviewFromDom(doc);
        });

        var cancelBtn = document.getElementById('imgpCancel');
        if (cancelBtn) cancelBtn.addEventListener('click', closeEditor);
        var saveBtn = document.getElementById('imgpSave');
        if (saveBtn) saveBtn.addEventListener('click', function () { onSave(doc, { apply: false }); });
        var saveApplyBtn = document.getElementById('imgpSaveApply');
        if (saveApplyBtn) saveApplyBtn.addEventListener('click', function () { onSave(doc, { apply: true }); });

        var overlay = document.getElementById('imgpEditorOverlay');
        if (overlay && overlay.addEventListener) {
            overlay.addEventListener('click', function (e) {
                if (e.target === overlay) closeEditor();
            });
        }

        // Делегированный обработчик: удаление строки файла.
        if (host.addEventListener) {
            host.addEventListener('click', function (e) {
                var target = e.target;
                var btn = (target && target.closest) ? target.closest('[data-imgp-action="file-remove"]') : null;
                if (!btn) return;
                var row = btn.closest ? btn.closest('[data-imgp-file-row]') : null;
                var idx = row ? parseInt(row.getAttribute('data-imgp-file-row'), 10) : -1;
                var rows = readFilesFromDoc(doc);
                if (idx >= 0) rows.splice(idx, 1);
                if (!rows.length) rows = [{ role: 'diffusion', repo: '', filename: '', revision: 'main' }];
                fillFiles(rows);
                refreshPreviewFromDom(doc);
            });
        }

        // Показываем serverArgs, если сервер их уже отдал (GET профиля).
        if (opts.serverArgs && opts.serverArgs.length) {
            var box = document.getElementById('imgpServerArgs');
            if (box) {
                box.innerHTML = '<div class="imgp-hint">' + escapeHtml(t('image.profiles.server_args_saved', 'Flags stored by the server for this profile:')) + '</div>' +
                    '<pre class="imgp-args">' + escapeHtml(opts.serverArgs.join(' ')) + '</pre>';
            }
        }
        return doc;
    }

    function applyFamilyDefaultsToDoc(doc, family) {
        var fam = familyDefaultsFor(family);
        var map = {
            imgpSteps: fam.steps,
            imgpCfg: fam.cfgScale,
            imgpWidth: fam.width,
            imgpHeight: fam.height,
            imgpBatch: fam.batchCount,
            imgpSampler: fam.sampler,
            imgpScheduler: fam.scheduler
        };
        Object.keys(map).forEach(function (id) {
            var node = doc.querySelector('[data-imgp-field="' + id + '"]');
            if (!node) return;
            var raw = String(node.value === undefined ? '' : node.value).trim();
            if (raw === '') node.value = String(map[id]);
        });
    }

    function nextSuggestedRole(rows) {
        var used = {};
        (rows || []).forEach(function (r) { used[r.role] = true; });
        for (var i = 0; i < ROLE_ORDER.length; i++) {
            if (!used[ROLE_ORDER[i]]) return ROLE_ORDER[i];
        }
        return 'lora';
    }

    function onFillFromCatalog(doc) {
        var sel = document.getElementById('imgpPreset');
        var name = sel ? sel.value : '';
        if (!name) { toast(t('image.profiles.preset_required', 'Select a catalog preset first'), 'warn'); return; }
        var found = null;
        state.catalog.forEach(function (c) { if (c.name === name) found = c; });
        if (!found) return;
        var form = profileToForm(found.profile, true);
        // Имя: у нового профиля оставляем то, что ввёл оператор (или имя пресета).
        var nameNode = doc.querySelector('[data-imgp-field="imgpName"]');
        var keepName = nameNode && String(nameNode.value || '').trim();
        var presetName = String(found.profile.name || found.name || '');
        fillField(doc, 'imgpName', keepName || presetName);
        fillField(doc, 'imgpFamily', form.family);
        fillField(doc, 'imgpSteps', form.defaultsSteps);
        fillField(doc, 'imgpCfg', form.defaultsCfgScale);
        fillField(doc, 'imgpWidth', form.defaultsWidth);
        fillField(doc, 'imgpHeight', form.defaultsHeight);
        fillField(doc, 'imgpBatch', form.defaultsBatchCount);
        fillField(doc, 'imgpSeed', form.defaultsSeed);
        fillField(doc, 'imgpClipSkip', form.defaultsClipSkip);
        fillField(doc, 'imgpSampler', form.defaultsSampler);
        fillField(doc, 'imgpScheduler', form.defaultsScheduler);
        fillField(doc, 'imgpNegative', form.defaultsNegativePrompt);
        fillField(doc, 'imgpVram', form.vramEstimateMb);
        fillField(doc, 'imgpTimeout', form.timeoutSec);
        fillField(doc, 'imgpIdleUnload', form.idleUnloadMinutes);
        fillField(doc, 'imgpNotes', form.notes);
        fillFiles(form.files);
        syncRuntimeExclusives(doc);
        refreshPreviewFromDom(doc);
        toast(t('image.profiles.preset_applied', 'Preset applied to the form'), 'success');
    }

    function fillField(doc, id, value) {
        var node = doc.querySelector('[data-imgp-field="' + id + '"]');
        if (!node) return;
        if (value === null || value === undefined) value = '';
        node.value = String(value);
    }

    // =====================================================================
    // Сохранение / apply / удаление
    // =====================================================================

    async function onSave(doc, opts) {
        opts = opts || {};
        var formState = formStateFromDom(doc);
        var check = validateForm(formState);
        renderEditorErrors(check.errors);
        if (!check.ok) {
            toast(errorText(check.errors[0]), 'error');
            return { ok: false, errors: check.errors };
        }
        var name = String(formState.name || '').trim();
        var body = formToProfile(formState);
        try {
            await api.put(name, body);
        } catch (e) {
            loadProfiles();
            toast(t('image.profiles.save_failed', 'Failed to save profile: {error}', { error: (e && e.message) || String(e) }), 'error');
            return { ok: false, error: (e && e.message) || String(e) };
        }
        closeEditor();
        await loadProfiles();
        toast(t('image.profiles.saved', 'Profile saved: {name}', { name: name }), 'success');
        if (opts.apply) await applyProfileWithProgress(name, { skipConfirm: true });
        return { ok: true, name: name, body: body };
    }

    /** Применение с опросом прогресса: прогресс image-apply = проверка воркеров. */
    async function applyProfileWithProgress(name, opts) {
        opts = opts || {};
        stopApplyPolling();
        state.applied[name] = { outcome: 'running', applyId: '', unreachable: 0, error: '', at: Date.now() };
        renderProfiles();
        var resp;
        try {
            resp = await api.apply(name, null);
        } catch (e) {
            state.applied[name] = {
                outcome: 'error', applyId: '', unreachable: 0,
                error: (e && e.message) || String(e), at: Date.now()
            };
            renderProfiles();
            toast(t('image.profiles.apply_failed', 'Failed to apply profile: {error}', { error: (e && e.message) || String(e) }), 'error');
            return { ok: false, error: (e && e.message) || String(e) };
        }
        var snap = normalizeApplyResponse(resp);
        state.applied[name] = {
            outcome: snap.outcome, applyId: snap.applyId,
            unreachable: snap.unreachable, error: snap.error, at: Date.now()
        };
        renderProfiles();
        notifyApplyOutcome(name, snap);
        if (snap.applyId && (snap.running || !snap.done)) {
            state.applyPoll = { name: name, applyId: snap.applyId, count: 0 };
        }
        return { ok: snap.outcome === 'applied', snapshot: snap };
    }

    function notifyApplyOutcome(name, snap) {
        if (snap.outcome === 'applied') {
            toast(t('image.profiles.apply_done', 'Profile applied on {n} worker(s): {name}',
                { n: snap.persisted, name: name }), 'success');
            return;
        }
        if (snap.outcome === 'unreachable') {
            toast(t('image.profiles.apply_unreachable', 'Profile saved, but no image worker answered: the running model keeps its old settings ({name}).', { name: name }), 'warn');
            return;
        }
        if (snap.outcome === 'partial') {
            var failedList = snap.backends.filter(function (b) { return b.status !== 'persisted'; })
                .map(function (b) { return b.backendId + ': ' + (b.status || '?'); }).join(', ');
            toast(t('image.profiles.apply_partial', 'Profile saved, but not applied everywhere: {list}', { list: failedList }), 'warn');
            return;
        }
        if (snap.outcome === 'unknown') {
            toast(t('image.profiles.apply_unknown', 'Profile saved. No image backend is registered (nothing to apply to).'), 'info');
        }
    }

    function stopApplyPolling() {
        if (state.applyPoll && state.applyPoll.timer) clearInterval(state.applyPoll.timer);
        state.applyPoll = null;
    }

    /** Один шаг опроса apply-прогресса (выделен, чтобы тест гонял его напрямую). */
    async function pollApplyOnce(job) {
        if (!job) return null;
        var current = state.applyPoll;
        try {
            var data = job.applyId
                ? await api.applyProgress(job.name, job.applyId)
                : await api.applyProgress(job.name, '');
            var snap = normalizeApplyResponse(data);
            state.applied[job.name] = {
                outcome: snap.outcome, applyId: snap.applyId || job.applyId,
                unreachable: snap.unreachable, error: snap.error, at: Date.now()
            };
            renderProfiles();
            if (snap.running || !snap.done) return snap;
            stopApplyPolling();
            notifyApplyOutcome(job.name, snap);
            return snap;
        } catch (e) {
            // 404 = трекер не помнит такой apply: считаем применение завершённым
            // (результат уже пришёл в ответе POST), а не крутим вечный таймер.
            stopApplyPolling();
            if (!isMissingEndpoint(e)) {
                state.applied[job.name] = {
                    outcome: 'error', applyId: job.applyId, unreachable: 0,
                    error: (e && e.message) || String(e), at: Date.now()
                };
                renderProfiles();
                // Устаревший ответ не должен ругаться после закрытия редактора или
                // запуска другого apply: проверяем, что это всё ещё наш job.
                if (!current || current === job) {
                    toast(t('image.profiles.apply_failed', 'Failed to apply profile: {error}', { error: (e && e.message) || String(e) }), 'error');
                }
            }
            return null;
        }
    }

    function startApplyPolling(name, applyId) {
        stopApplyPolling();
        // job держим в state.applyPoll ДО первого тика: иначе мгновенный ответ
        // (done) вызвал бы stopApplyPolling, который не нашёл бы job.timer.
        var job = { name: name, applyId: applyId, count: 0, timer: null };
        state.applyPoll = job;
        var tick = function () {
            job.count++;
            if (job.count > APPLY_POLL_MAX) { stopApplyPolling(); return; }
            pollApplyOnce(job);
        };
        job.timer = setInterval(tick, APPLY_POLL_MS);
        tick();
        return job;
    }

    async function onDelete(name) {
        var msg = t('image.profiles.confirm_delete', 'Delete profile "{name}"?', { name: name });
        var ok = (typeof window.confirm === 'function')
            ? window.confirm(msg)
            : (typeof confirm === 'function' ? confirm(msg) : true);
        if (!ok) return { ok: false, cancelled: true };
        stopApplyPolling();
        delete state.applied[name];
        try {
            await api.remove(name);
        } catch (e) {
            toast(t('image.profiles.delete_failed', 'Failed to delete profile: {error}', { error: (e && e.message) || String(e) }), 'error');
            return { ok: false, error: (e && e.message) || String(e) };
        }
        await loadProfiles();
        toast(t('image.profiles.deleted', 'Profile deleted: {name}', { name: name }), 'success');
        return { ok: true };
    }

    async function onEdit(name) {
        var entry = findEntry(name);
        if (!entry) return null;
        var profile = entry.profile;
        var serverArgs = null;
        try {
            var detail = await api.get(name);
            if (detail && detail.profile) profile = detail.profile;
            if (detail && Array.isArray(detail.serverArgs)) serverArgs = detail.serverArgs;
        } catch (e) {
            // Список уже отдал профиль - 404/сеть не должны мешать правке.
            if (!isMissingEndpoint(e)) {
                toast(t('image.profiles.detail_failed', 'Could not load the profile details, editing the list copy: {error}', { error: (e && e.message) || String(e) }), 'warn');
            }
        }
        return openEditor(profile, { isNew: false, serverArgs: serverArgs });
    }

    function findEntry(name) {
        for (var i = 0; i < state.profiles.length; i++) {
            if (state.profiles[i].name === name) return state.profiles[i];
        }
        return null;
    }

    /** Уникальное имя для копии: name-copy, name-copy-2, ... */
    function uniqueCopyName(name) {
        var base = String(name || 'profile') + '-copy';
        var candidate = base;
        var n = 2;
        while (findEntry(candidate)) { candidate = base + '-' + n; n++; }
        return candidate;
    }

    function onDuplicate(name) {
        var entry = findEntry(name);
        if (!entry) return null;
        var copy = JSON.parse(JSON.stringify(entry.profile));
        return openEditor(copy, { isNew: true, name: uniqueCopyName(name) });
    }

    // =====================================================================
    // События страницы
    // =====================================================================

    function onClick(e) {
        var target = e.target;
        var actionEl = (target && target.closest) ? target.closest('[data-imgp-action]') : null;
        if (!actionEl) return;
        var action = actionEl.getAttribute('data-imgp-action');
        var name = actionEl.getAttribute('data-name') || '';
        if (action === 'edit') onEdit(name);
        else if (action === 'duplicate') onDuplicate(name);
        else if (action === 'apply') applyProfileWithProgress(name, {});
        else if (action === 'delete') onDelete(name);
        else if (action === 'new') openEditor(null, { isNew: true, name: '' });
    }

    /**
     * Монтирование модуля в контейнер секции профилей (#imageProfiles).
     *
     * Точка входа - init() в image-page.js: контейнер объявлен в webui/index.html
     * внутри #image-page, поэтому здесь только подписка и первичный refresh.
     */
    function mount(containerOrId) {
        var node = null;
        if (containerOrId && typeof containerOrId === 'object') node = containerOrId;
        else if (typeof containerOrId === 'string') node = document.getElementById(containerOrId);
        if (!node) node = el('imageProfiles');
        if (!node) return false;
        state.container = node;
        if (!state.bound && node.addEventListener) {
            state.bound = true;
            node.addEventListener('click', onClick);
        }
        var refreshBtn = el('imageProfilesRefreshBtn');
        if (refreshBtn && !refreshBtn._imgpBound) {
            refreshBtn._imgpBound = true;
            refreshBtn.addEventListener('click', function () { refresh(); });
        }
        var newBtn = el('imageProfilesNewBtn');
        if (newBtn && !newBtn._imgpBound) {
            newBtn._imgpBound = true;
            newBtn.addEventListener('click', function () { openEditor(null, { isNew: true, name: '' }); });
        }
        if (!state.mounted) {
            state.mounted = true;
            if (window.I18N && typeof window.I18N.getLang === 'function') {
                window.addEventListener('i18n:changed', function () {
                    renderProfiles();
                    renderNotice();
                });
            }
        }
        return true;
    }

    // =====================================================================
    // Публичный API
    // =====================================================================

    window.ImageProfiles = {
        mount: mount,
        refresh: refresh,
        // Экспорт для тестов: чистые функции + DOM-хелперы разбора формы
        // (node webui/js/modules/image-profiles.test.js).
        pure: pure,
        _state: state,
        _api: api,
        _actions: {
            refresh: refresh,
            loadProfiles: loadProfiles,
            loadCatalog: loadCatalog,
            openEditor: openEditor,
            closeEditor: closeEditor,
            save: onSave,
            apply: applyProfileWithProgress,
            applyThenPoll: function (name) {
                return applyProfileWithProgress(name, {}).then(function (res) {
                    if (res && res.snapshot && res.snapshot.applyId) {
                        return startApplyPolling(name, res.snapshot.applyId);
                    }
                    return null;
                });
            },
            pollApplyOnce: pollApplyOnce,
            delete: onDelete,
            edit: onEdit,
            duplicate: onDuplicate,
            readFormFromDoc: readFormFromDoc,
            formStateFromDom: formStateFromDom,
            previewServerArgs: previewServerArgs,
            editorHtmlForTest: editorHtml,
            onClick: onClick,
            stopPolling: stopApplyPolling
        }
    };
})();
