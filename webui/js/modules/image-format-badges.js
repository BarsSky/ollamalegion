// image-format-badges.js — R755 (2026-11-25): бейджи форматов файлов.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ СЛОЙ. Один и тот же вопрос «примет ли движок этот файл»
// возникает в двух местах: в списке файлов HuggingFace (до скачивания) и в
// панели файлов уже скачанной группы. Сами ПРАВИЛА приходят с сервера
// (GET /api/v1/image/backends/{id}/formats → правила «формат × роль × семейство»),
// а здесь только:
//   - выбор вердикта для файла (предупреждение по имени MLX сильнее правила);
//   - HTML-бейдж и подсказка (переводы — из пакетов i18n);
//   - одна строка «Форматы: …» для карточки репозитория/панели.
//
// ЧЕГО ЗДЕСЬ НЕТ: знания о семействах и форматах. Если бы таблица «Qwen-Image
// diffusion — только GGUF» жила в JS, она разъехалась бы с probe/compat воркера
// (они читают реальные заголовки). Здесь только отрисовка машинных кодов:
// reads | needs_gguf | warn | unsupported | unknown.
(function () {
    'use strict';

    var VERDICT_LEVEL = {
        reads: 'ok',
        needs_gguf: 'needs',
        warn: 'warn',
        unsupported: 'bad',
        unknown: 'unknown'
    };
    var VERDICT_ICON = {
        ok: 'fa-circle-check',
        needs: 'fa-triangle-exclamation',
        warn: 'fa-circle-question',
        bad: 'fa-circle-xmark',
        unknown: 'fa-circle-question'
    };
    var VERDICT_KEY = {
        reads: 'imageModels.format_chip_reads',
        needs_gguf: 'imageModels.format_chip_needs_gguf',
        warn: 'imageModels.format_chip_warn',
        unsupported: 'imageModels.format_chip_unsupported',
        unknown: 'imageModels.format_chip_unknown'
    };
    var HINT_KEY = {
        reads: 'imageModels.format_hint_reads',
        needs_gguf: 'imageModels.format_hint_needs_gguf',
        warn: 'imageModels.format_hint_warn',
        unsupported: 'imageModels.format_hint_unsupported',
        unknown: 'imageModels.format_hint_unknown'
    };
    var FALLBACK = {
        reads: 'читается',
        needs_gguf: 'нужен GGUF',
        warn: 'проверьте формат',
        unsupported: 'не читается',
        unknown: 'формат не проверен'
    };
    var HINT_FALLBACK = {
        reads: 'Движок читает этот формат в этой роли.',
        needs_gguf: 'В этой роли движок читает только GGUF: возьмите GGUF-квант этого файла.',
        warn: 'Формат допустим не всегда: нажмите «Проверить» — воркер прочитает заголовок.',
        unsupported: 'Движок этот файл не прочитает. Для diffusion нужен GGUF; VAE и энкодеры в safetensors допустимы.',
        unknown: 'Данных о формате нет: прочитайте заголовок кнопкой «Проверить».'
    };

    function t(key, fallback, vars) {
        if (window.I18N && typeof window.I18N.t === 'function') {
            var v = vars ? window.I18N.t(key, vars) : window.I18N.t(key);
            if (v && v !== key) return v;
        }
        var base = fallback !== undefined ? fallback : key;
        if (vars) {
            base = String(base).replace(/\{(\w+)\}/g, function (m, k) {
                return Object.prototype.hasOwnProperty.call(vars, k) ? String(vars[k]) : m;
            });
        }
        return base;
    }

    function escapeHtml(text) {
        if (text === null || text === undefined) return '';
        return String(text)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }

    /** fileExt — расширение файла в нижнем регистре (без точки). */
    function fileExt(filename) {
        var s = String(filename || '').replace(/\\/g, '/');
        var base = s.slice(s.lastIndexOf('/') + 1);
        var i = base.lastIndexOf('.');
        return i >= 0 ? base.slice(i + 1).toLowerCase() : '';
    }

    /**
     * resolve — вердикт для файла по матрице воркера.
     *
     * Порядок: предупреждение по ИМЕНИ (MLX) сильнее правила формата, потому что
     * имя известно до скачивания и предупреждение как раз для того, чтобы не
     * качать гигабайты. Точный вердикт всё равно остаётся за probe.
     */
    function resolve(matrix, filename, role) {
        if (!matrix || matrix.error) return null;
        var nameWarning = findNameWarning(matrix, filename);
        if (nameWarning) {
            return { verdict: nameWarning.verdict, code: nameWarning.code || '', byName: true };
        }
        var ext = fileExt(filename);
        if (!ext) return null;
        role = String(role || '');
        var rules = Array.isArray(matrix.rules) ? matrix.rules : [];
        var exact = null;
        var fallback = null;
        rules.forEach(function (r) {
            if (!r || r.format !== ext) return;
            if (r.role === role) exact = r;
            else if (r.role === '*') fallback = r;
        });
        var hit = exact || fallback;
        return hit ? { verdict: hit.verdict, code: '', byName: false } : null;
    }

    /** findNameWarning — предупреждение по имени файла из матрицы. */
    function findNameWarning(matrix, filename) {
        if (!matrix || !Array.isArray(matrix.nameWarnings)) return null;
        var base = String(filename || '').replace(/\\/g, '/').toLowerCase();
        base = base.slice(base.lastIndexOf('/') + 1);
        for (var i = 0; i < matrix.nameWarnings.length; i++) {
            var w = matrix.nameWarnings[i] || {};
            if (w.contains && base.indexOf(String(w.contains).toLowerCase()) !== -1) return w;
        }
        return null;
    }

    /** badgeHtml — бейдж «расширение · вердикт» с подсказкой. */
    function badgeHtml(matrix, filename, role) {
        var hit = resolve(matrix, filename, role);
        if (!hit) return '';
        var verdict = String(hit.verdict || 'unknown');
        var level = VERDICT_LEVEL[verdict] || 'unknown';
        var label = hit.byName
            ? t('imageModels.format_chip_mlx', 'MLX-квант: не читается')
            : t(VERDICT_KEY[verdict] || VERDICT_KEY.unknown, FALLBACK[verdict] || FALLBACK.unknown);
        var hint = hit.byName
            ? t('imageModels.format_hint_mlx',
                'MLX-квант safetensors (веса U32) движок не читает. Для diffusion возьмите GGUF; VAE и энкодеры в safetensors допустимы.')
            : t(HINT_KEY[verdict] || HINT_KEY.unknown, HINT_FALLBACK[verdict] || HINT_FALLBACK.unknown);
        var ext = fileExt(filename);
        return '<span class="ifb-badge ifb-' + level + '" title="' + escapeHtml(hint) + '">' +
            '<i class="fas ' + (VERDICT_ICON[level] || VERDICT_ICON.unknown) + '"></i> ' +
            escapeHtml(ext) + ' · ' + escapeHtml(label) + '</span>';
    }

    /** summaryText — одна строка «Форматы: …» для семейства. */
    function summaryText(matrix) {
        if (!matrix || matrix.error || !Array.isArray(matrix.rules)) return '';
        var rules = matrix.rules;
        var find = function (format, role) {
            var exact = null, any = null;
            rules.forEach(function (r) {
                if (!r || r.format !== format) return;
                if (r.role === role) exact = r;
                else if (r.role === '*') any = r;
            });
            return (exact || any || {}).verdict || '';
        };
        var parts = [];
        if (find('gguf', '*') === 'reads') {
            parts.push(t('imageModels.format_summary_gguf', 'GGUF - читается'));
        }
        var st = find('safetensors', 'diffusion');
        if (st === 'reads') {
            parts.push(t('imageModels.format_summary_safetensors_allinone',
                'safetensors all-in-one - читается'));
        } else if (st === 'needs_gguf') {
            parts.push(t('imageModels.format_summary_safetensors_needs_gguf',
                'safetensors diffusion - нужен GGUF'));
        } else if (st === 'warn') {
            parts.push(t('imageModels.format_summary_safetensors_warn',
                'safetensors diffusion - решает заголовок («Проверить»)'));
        }
        if (find('safetensors', 'vae') === 'reads') {
            parts.push(t('imageModels.format_summary_components',
                'VAE/энкодеры в safetensors - читаются'));
        }
        if (!parts.length) return '';
        return t('imageModels.format_summary_prefix', 'Форматы') + ': ' + parts.join('; ') + '.';
    }

    /** mlxWarningHtml — предупреждение, если в наборе есть MLX-подобные имена. */
    function mlxWarningHtml(matrix, filenames) {
        if (!matrix || !Array.isArray(matrix.nameWarnings)) return '';
        var hit = false;
        (filenames || []).forEach(function (name) {
            if (!hit && findNameWarning(matrix, name)) hit = true;
        });
        if (!hit) return '';
        return '<div class="ifb-warning"><i class="fas fa-triangle-exclamation"></i> ' +
            escapeHtml(t('imageModels.format_mlx_warning',
                'В наборе есть файлы с «MLX» в имени: это MLX-квант safetensors (веса U32), движок его не читает. Для diffusion возьмите GGUF; VAE и энкодеры в safetensors допустимы.')) +
            '</div>';
    }

    window.ImageFormatBadges = {
        resolve: resolve,
        findNameWarning: findNameWarning,
        badgeHtml: badgeHtml,
        summaryText: summaryText,
        mlxWarningHtml: mlxWarningHtml,
        fileExt: fileExt,
        // Для тестов: какие коды контракта поддерживаются.
        VERDICTS: ['reads', 'needs_gguf', 'warn', 'unsupported', 'unknown']
    };
})();
