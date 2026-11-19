/**
 * image-profile-pure.js — R640 (2026-11-19): чистый слой редактора профилей
 * image-моделей (без DOM, без сети, без i18n).
 *
 * ЗАЧЕМ ВЫНЕСЕНО. `image-profiles.js` держал 1830 строк: чистые правила (контракт
 * pkg/types/image_model.go, разбор формы, сборка тела PUT, нормализация ответов API)
 * и рядом — весь DOM-редактор. Чистый слой покрыт отдельным набором
 * (`image-profiles.test.js`) и не меняется при правках разметки; держать его в
 * конце 1800-строчного файла — значит искать «где правило имени профиля» прокруткой.
 *
 * КАК СВЯЗАНО. Этот файл обязан грузиться ПЕРЕД `image-profiles.js`
 * (`webui/index.html`): тот берёт `window.ImageProfilePure` и раздаёт имена
 * локальными ссылками, поэтому остальной код модуля не менялся ни на строку.
 * Публичная точка входа осталась одна — `window.ImageProfiles`.
 *
 * ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: код перенесён как есть.
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

    var APPLY_POLL_MS = 1200;
    var APPLY_POLL_MAX = 40;


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

    window.ImageProfilePure = pure;
})();
