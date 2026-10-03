/**
 * image-models-hf.js — R-Image Phase 9: табы «HuggingFace» и «Загрузки»
 * страницы «Image-модели» (window.ImageModelsHf).
 *
 * ЧТО ЭТО. Модуль табов HF/Загрузки: поиск репозиториев на HuggingFace, выбор
 * файлов модели (bundle) с ПРЕДЛОЖЕННЫМИ сервером ролями, запуск bundle-загрузки
 * и страница состояния загрузок (активные bundle, история, остатки).
 *
 * ПОЧЕМУ В СТИЛЕ GGUF. Страница «GGUF модели» уже решает ту же задачу для
 * llama.cpp (поиск → файлы → скачивание → прогресс → история → остатки), и
 * оператору не нужно учить второй интерфейс. Поэтому здесь ТОТ ЖЕ визуальный
 * язык (классы .gguf-search-form/.gguf-result-card/.gguf-file-item/
 * .gguf-download-item/.gguf-empty-state из webui/css/pages.css) и те же готовые
 * ключи i18n там, где смысл совпадает (gguf.search_btn, gguf.delete_from_disk,
 * gguf.active_downloads, gguf.download_history, gguf.orphans_hint, ...).
 *
 * ГРАНИЦЫ (Phase 9): модуль владеет ТОЛЬКО табами hf/downloads. Список «Модели на
 * диске», «Загруженные» и «Настройки» рисует image-page.js (у него уже есть
 * рабочий код и тесты), шелл image-models-page.js даёт контекст.
 *
 * ЧЕГО ЗДЕСЬ НЕТ: генерации и показа картинок. WebUI — для настройки и состояния.
 *
 * РОЛИ ФАЙЛОВ приходят с СЕРВЕРА (поле suggestedRole в GET /api/hf/files,
 * internal/sdbackend/models.go → SuggestRole). В JS эвристика НЕ дублируется:
 * иначе правила «какой файл есть VAE» разъехались бы между воркером и UI, и
 * bundle собирался бы неправильно при том же имени файла.
 *
 * Экспорт: window.ImageModelsHf = { mount, render, tabIds, _actions, _state, pure }.
 */
(function () {
    'use strict';

    var TAB_IDS = ['hf', 'downloads'];
    var HF_TOKEN_KEY = 'ollamalegion_hf_token';

    // Таймауты: управляющие запросы к воркеру быстрые, но поиск HF ходит в
    // интернет, поэтому у него свой, более щедрый лимит.
    var TIMEOUT_CONTROL_MS = 15000;
    var TIMEOUT_SEARCH_MS = 30000;
    var DEFAULT_POLL_MS = 2000;
    var SEARCH_LIMIT = 20;

    // Роли файлов — ЗАМОРОЖЕННЫЙ контракт pkg/types/image_model.go:32-45.
    var IMAGE_ROLES = ['diffusion', 'vae', 'clip_l', 'clip_g', 't5xxl', 'llm', 'clip_vision', 'taesd', 'lora', 'upscaler', 'controlnet', 'ip_adapter'];
    // Семейства — pkg/types/image_model.go:49-52.
    var IMAGE_FAMILIES = ['sd15', 'sd21', 'sd_turbo', 'sdxl', 'sdxl_turbo', 'sd3', 'flux', 'flux2', 'chroma', 'qwen_image', 'z_image', 'other'];

    /**
     * Роли, которые отмечаются галочкой автоматически: почти любой bundle для
     * sd.cpp — это diffusion + VAE + text encoder'ы.
     *
     * t5xxl/llm СОЗНАТЕЛЬНО не отмечаются: это отдельные файлы на 3-9 ГБ, и
     * скачать их «за компанию» по ошибке дороже, чем поставить галочку руками.
     * Роль при этом всё равно подставлена из suggestedRole — оператор видит,
     * что это text encoder, и решает сам.
     */
    var AUTO_ROLES = ['diffusion', 'vae', 'clip_l', 'clip_g'];

    var mounted = false;
    var state = {
        ctx: null,
        query: '',
        task: 'text-to-image',
        searching: false,
        searchError: '',
        results: [],
        selectedRepo: '',
        files: [],
        filesLoading: false,
        filesError: '',
        // path → { checked: bool, role: string }
        selection: {},
        // path → результат пред-проверки заголовка (/api/hf/probe): вердикт
        // «прочитает ли движок». Ключ — путь файла в репозитории.
        probes: {},
        // path → true, пока идёт пред-проверка (для спиннера в строке файла).
        probing: {},
        bundleName: '',
        bundleFamily: 'sd15',
        // familyManual — оператор сам выбирал семейство в селекте: автоподстановка
        // по заголовку файла (applyProbedFamily) после этого молчит.
        familyManual: false,
        // familyNotice — сообщение «семейство подставлено по файлу» для шапки.
        familyNotice: '',
        tokenSaved: false,
        // прогресс bundle: снимок HFBundleProgress из /api/hf/progress?bundleId=
        bundle: null,
        poll: null,
        pollIdleTicks: 0,
        downloads: null,
        downloadsError: '',
        busy: false
    };

    // =====================================================================
    // Pure helpers — без DOM, покрыты node-тестом
    // (webui/js/modules/image-models-hf.test.js).
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

    function formatBytes(n) {
        if (n === null || n === undefined || n === '' || isNaN(Number(n))) return '-';
        var v = Number(n);
        if (v <= 0) return '0 B';
        var units = ['B', 'KB', 'MB', 'GB', 'TB'];
        var i = 0;
        while (v >= 1024 && i < units.length - 1) { v = v / 1024; i++; }
        return (i === 0 ? String(Math.round(v)) : v.toFixed(v >= 100 ? 0 : 1)) + ' ' + units[i];
    }

    /** Имя файла: воркер отдаёт path, raw HF API — rfilename. */
    function filePath(f) {
        f = f || {};
        return String(f.path || f.rfilename || f.filename || f.name || '');
    }

    function fileSize(f) {
        f = f || {};
        return Number(f.sizeBytes || f.size_bytes || f.size || 0);
    }

    /**
     * Роль файла: ТОЛЬКО серверная подсказка (suggestedRole). Пустая строка =
     * «сервер не знает» (тогда UI честно предлагает first-role из списка и
     * оператор выбирает сам).
     */
    function suggestedRole(f) {
        f = f || {};
        return String(f.suggestedRole || f.suggested_role || '');
    }

    function isValidRole(role) {
        return IMAGE_ROLES.indexOf(String(role || '')) !== -1;
    }

    /**
     * Отметки по умолчанию для списка файлов репозитория.
     *
     * ПРАВИЛА (объяснены в комментарии к AUTO_ROLES):
     *   - файл с ролью из AUTO_ROLES отмечается, если для этой роли ещё нет
     *     отмеченного файла (первый по порядку в репозитории — как правило,
     *     основной вариант, а не fp16-дубль);
     *   - t5xxl/llm/lora/controlnet/... — не отмечаются;
     *   - preserve (предыдущие выборы пользователя) имеет приоритет: повторный
     *     рендер (смена языка, refresh) не должен сбрасывать галочки.
     */
    function defaultSelection(files, preserve) {
        var prev = preserve || {};
        var sel = {};
        var autoUsed = {};
        (files || []).forEach(function (f) {
            var p = filePath(f);
            if (!p) return;
            if (Object.prototype.hasOwnProperty.call(prev, p)) {
                sel[p] = {
                    checked: !!prev[p].checked,
                    role: prev[p].role || suggestedRole(f) || 'diffusion',
                    // sizeBytes тащим в выбор: он нужен buildFileSpecs, чтобы
                    // воркер не перекачивал уже лежащие файлы (hf_bundle.go:128).
                    sizeBytes: fileSize(f)
                };
                autoUsed[sel[p].role] = true;
                return;
            }
            var role = suggestedRole(f) || 'diffusion';
            var auto = AUTO_ROLES.indexOf(role) !== -1 && !autoUsed[role];
            if (auto) autoUsed[role] = true;
            sel[p] = { checked: auto, role: role, sizeBytes: fileSize(f) };
        });
        return sel;
    }

    /**
     * Отметки → спецификация bundle для POST /api/hf/bundle.
     *
     * Тело: {name, family, files:[{role, repo, filename, revision}]}
     * (cmd/sdworker/handlers_hf.go:45-48, hfBundleRequest).
     *
     * Ошибки — КОДАМИ (не текстом): pure-слой не зависит от языка.
     *   need_name      — имя bundle не задано;
     *   empty          — ни один файл не отмечен;
     *   need_diffusion — нет роли diffusion (без него sd-server не стартует);
     *   dup_role       — роль занята дважды (кроме lora: их может быть много).
     */
    function buildFileSpecs(repo, selection, opts) {
        opts = opts || {};
        var errors = [];
        var name = String(opts.name || '').trim();
        if (!name) errors.push({ code: 'need_name' });
        var files = [];
        var seen = {};
        Object.keys(selection || {}).forEach(function (path) {
            var item = selection[path];
            if (!item || !item.checked) return;
            var role = String(item.role || '');
            if (!isValidRole(role)) {
                errors.push({ code: 'bad_role', path: path, role: role });
                return;
            }
            // Дубликат роли запрещён, кроме lora (pkg/types/image_model.go:
            // "duplicate role" только для остальных).
            if (seen[role] && role !== 'lora') {
                errors.push({ code: 'dup_role', role: role, path: path });
                return;
            }
            seen[role] = true;
            files.push({
                role: role,
                repo: String(repo || ''),
                filename: path,
                revision: 'main',
                sizeBytes: Number((item && item.sizeBytes) || 0)
            });
        });
        if (!files.length) errors.push({ code: 'empty' });
        if (files.length && !seen.diffusion) errors.push({ code: 'need_diffusion' });
        return { name: name, files: files, errors: errors };
    }

    /** Первая ошибка сборки → ключ i18n + параметры (для тоста). */
    function bundleErrorToI18n(err) {
        err = err || {};
        if (err.code === 'need_name') return { key: 'image.bundle_need_name', fallback: 'Bundle name is required', vars: {} };
        if (err.code === 'empty') return { key: 'imageModels.hf_none_selected', fallback: 'Select at least one file', vars: {} };
        if (err.code === 'need_diffusion') return { key: 'image.bundle_need_diffusion', fallback: 'A diffusion file is required in the bundle', vars: {} };
        if (err.code === 'dup_role') return { key: 'image.bundle_dup_role', fallback: 'Duplicate role {role}', vars: { role: err.role } };
        if (err.code === 'bad_role') return { key: 'imageModels.hf_bad_role', fallback: 'Unknown role: {role}', vars: { role: err.role } };
        return { key: 'common.error', fallback: 'Error', vars: {} };
    }

    /**
     * Процент по снимку прогресса: сервер отдаёт progressPct, но для снимков
     * bundle-истории он может быть 0 при completed — тогда считаем 100.
     */
    function progressPercent(p) {
        p = p || {};
        var status = String(p.status || '');
        if (status === 'completed') return 100;
        var pct = Number(p.progressPct || p.progress_pct || 0);
        if (!pct && p.totalBytes > 0) pct = Math.floor((Number(p.downloaded || 0) / Number(p.totalBytes)) * 100);
        if (pct > 100) pct = 100;
        if (pct < 0 || isNaN(pct)) pct = 0;
        return Math.round(pct);
    }

    /** Ключ i18n состояния загрузки bundle (переиспользуем gguf.* там, где есть). */
    function bundleStatusKey(status) {
        var s = String(status || '').toLowerCase();
        if (s === 'downloading') return 'gguf.downloading';
        if (s === 'completed') return 'gguf.download_completed';
        if (s === 'failed') return 'gguf.download_failed';
        if (s === 'cancelled') return 'gguf.download_cancelled';
        if (s === 'interrupted') return 'imageModels.status_interrupted';
        return 'common.unknown';
    }

    /**
     * Сводка состава download-снимка: нужна «Загрузкам», чтобы не рисовать
     * пустые секции (в стиле GGUF: история есть — секция есть).
     */
    function downloadsSummary(snap) {
        snap = snap || {};
        var bundles = Array.isArray(snap.bundles) ? snap.bundles : [];
        var bundleHistory = Array.isArray(snap.bundleHistory) ? snap.bundleHistory : [];
        var active = Array.isArray(snap.active) ? snap.active : [];
        var history = Array.isArray(snap.history) ? snap.history : [];
        var orphans = Array.isArray(snap.orphans) ? snap.orphans : [];
        return {
            bundles: bundles,
            bundleHistory: bundleHistory,
            active: active,
            history: history,
            orphans: orphans,
            hasAny: !!(bundles.length || bundleHistory.length || active.length || history.length || orphans.length),
            activeCount: bundles.length + active.length
        };
    }

    // =====================================================================
    // Контекст и запросы
    // =====================================================================

    function t(key, fallback, vars) {
        if (window.I18N && typeof window.I18N.t === 'function') {
            var v = vars ? window.I18N.t(key, vars) : window.I18N.t(key);
            if (v && v !== key) return v;
        }
        if (fallback && vars) {
            return String(fallback).replace(/\{(\w+)\}/g, function (m, k) {
                return Object.prototype.hasOwnProperty.call(vars, k) ? String(vars[k]) : m;
            });
        }
        return fallback || key;
    }

    function el(id) {
        return (typeof document !== 'undefined' && document.getElementById) ? document.getElementById(id) : null;
    }

    function apiBase() {
        if (state.ctx && state.ctx.apiBase) return String(state.ctx.apiBase).replace(/\/+$/, '');
        var cfg = window.WEBUI_CONFIG || {};
        var base = cfg.API_BASE_URL || cfg.API_BASE ||
            (typeof window !== 'undefined' && window.location && window.location.origin) || '';
        return String(base).replace(/\/+$/, '');
    }

    function authHeaders(extra) {
        var headers = (window.Api && typeof window.Api.getAuthHeaders === 'function')
            ? window.Api.getAuthHeaders()
            : { 'Content-Type': 'application/json' };
        if (!headers['X-API-Token']) {
            var tk = (window.WEBUI_CONFIG && window.WEBUI_CONFIG.API_TOKEN) || '';
            if (!tk) {
                try { tk = localStorage.getItem('apiToken') || ''; } catch (e) { tk = ''; }
            }
            if (tk) headers['X-API-Token'] = tk;
        }
        if (extra) Object.keys(extra).forEach(function (k) { headers[k] = extra[k]; });
        return headers;
    }

    function hfToken() {
        try { return localStorage.getItem(HF_TOKEN_KEY) || ''; } catch (e) { return ''; }
    }

    function saveHfToken(value) {
        try {
            if (value) localStorage.setItem(HF_TOKEN_KEY, value);
            else localStorage.removeItem(HF_TOKEN_KEY);
        } catch (e) { /* приватный режим */ }
        if (window.GgufApi && typeof window.GgufApi.setHFToken === 'function') {
            try { window.GgufApi.setHFToken(value || ''); } catch (e2) { /* необязательный мост */ }
        }
    }

    function backendId() {
        return state.ctx && state.ctx.backendId ? String(state.ctx.backendId) : '';
    }

    /** Путь к воркеру через балансер: /api/v1/image/backends/{id}/<rest>. */
    function backendUrl(rest) {
        return apiBase() + '/api/v1/image/backends/' + encodeURIComponent(backendId()) + '/' + rest;
    }

    /** fetch + JSON с разбором конверта ошибки воркера ({error: "..."}). */
    function request(url, opts) {
        opts = opts || {};
        var timeoutMs = opts.timeoutMs || TIMEOUT_CONTROL_MS;
        var ctrl = (typeof AbortController !== 'undefined') ? new AbortController() : null;
        var timer = null;
        if (ctrl) timer = setTimeout(function () { ctrl.abort(); }, timeoutMs);
        var headers = authHeaders(opts.headers);
        // HF-токен шлём только HF-путям (как gguf-api.js:1015 и image-page.js:539):
        // воркер читает его из X-HF-Token (cmd/sdworker/handlers_hf.go:107).
        if (url.indexOf('/hf/') !== -1) {
            var tk = hfToken();
            if (tk) headers['X-HF-Token'] = tk;
        }
        var init = { method: opts.method || 'GET', headers: headers };
        if (ctrl) init.signal = ctrl.signal;
        if (opts.body !== undefined) init.body = opts.body;
        return fetch(url, init).then(function (resp) {
            if (timer) clearTimeout(timer);
            return resp.text().then(function (text) {
                var parsed = null;
                if (text) { try { parsed = JSON.parse(text); } catch (e) { parsed = text; } }
                if (!resp.ok) {
                    var msg = (parsed && (parsed.error || parsed.message)) || ('HTTP ' + resp.status);
                    var err = new Error(msg);
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

    function toast(msg, type) {
        if (window.Toast && typeof window.Toast.show === 'function') {
            window.Toast.show({ message: msg, type: type || 'info' });
            return;
        }
        if (window.Notifications && typeof window.Notifications.show === 'function') {
            window.Notifications.show(msg, type);
            return;
        }
        if (window.console) console.log('[image-models-hf]', type || 'info', msg);
    }

    // =====================================================================
    // Рендер: поиск на HuggingFace
    // =====================================================================

    function setHtml(id, html) {
        var node = el(id);
        if (node) node.innerHTML = html;
    }

    function renderNotice(id, text, isError) {
        var node = el(id);
        if (!node) return;
        if (!text) { node.style.display = 'none'; node.innerHTML = ''; return; }
        node.style.display = '';
        node.style.color = isError ? 'var(--danger)' : 'var(--text-muted)';
        node.innerHTML = isError
            ? '<i class="fas fa-exclamation-triangle"></i> ' + escapeHtml(text)
            : '<i class="fas fa-circle-info"></i> ' + escapeHtml(text);
    }

    function renderSearchResults() {
        var host = el('imHfSearchResults');
        if (!host) return;
        if (state.searching) {
            host.innerHTML = '<div class="gguf-empty-state"><i class="fas fa-spinner fa-spin"></i> ' +
                escapeHtml(t('gguf.searching_for', 'Searching for "{q}"...', { q: state.query })) + '</div>';
            return;
        }
        if (state.searchError) {
            host.innerHTML = '<div class="gguf-empty-state gguf-search-error">' +
                '<i class="fas fa-exclamation-triangle" style="color:var(--warning,#e0a030);font-size:18px;"></i>' +
                '<div style="margin-top:6px;font-weight:500;">' + escapeHtml(t('gguf.search_error', 'Search failed')) + '</div>' +
                '<div style="margin-top:4px;color:var(--text-muted);font-size:12px;">' + escapeHtml(state.searchError) + '</div>' +
                '</div>';
            return;
        }
        var list = Array.isArray(state.results) ? state.results : [];
        if (!list.length) {
            if (!state.query || state.query.trim().length < 2) {
                host.innerHTML = '<div class="gguf-empty-state gguf-search-empty" style="padding:12px 4px;">' +
                    '<i class="fab fa-huggingface" style="font-size:18px;color:#ff9a00;"></i> ' +
                    '<span style="font-weight:500;">' + escapeHtml(t('gguf.search_title', 'HuggingFace Hub')) + '</span>' +
                    '<div style="color:var(--text-muted);font-size:12px;margin-top:6px;">' +
                    escapeHtml(t('imageModels.hf_search_hint', 'Введите 2+ символа или org/model (со слешем) — сразу попадёте к файлам.')) +
                    '</div></div>';
                return;
            }
            host.innerHTML = '<div class="gguf-empty-state"><i class="fas fa-search" style="opacity:0.4;"></i>' +
                '<div style="margin-top:6px;">' + escapeHtml(t('gguf.no_results_for', 'No results for "{q}"', { q: state.query })) + '</div></div>';
            return;
        }
        var rows = list.map(function (m) {
            m = m || {};
            var id = String(m.id || m.modelId || m.name || '');
            var updates = m.lastModified || m.lastUpdated || '';
            var when = updates ? String(updates).slice(0, 10) : '-';
            var selected = id === state.selectedRepo;
            return '<div class="gguf-result-card' + (selected ? ' active' : '') + '" data-imh-repo="' + escapeHtml(id) + '">' +
                '<div class="gguf-result-header">' +
                    '<div class="gguf-result-title"><i class="fas fa-cube"></i> ' + escapeHtml(id) + '</div>' +
                    '<div class="gguf-result-meta">' +
                        '<span title="' + escapeHtml(t('gguf.downloads', 'Downloads')) + '"><i class="fas fa-download"></i> ' + Number(m.downloads || 0) + '</span>' +
                        '<span title="' + escapeHtml(t('gguf.likes', 'Likes')) + '"><i class="fas fa-heart"></i> ' + Number(m.likes || 0) + '</span>' +
                        '<span title="' + escapeHtml(t('gguf.last_updated', 'Updated')) + '"><i class="fas fa-calendar"></i> ' + escapeHtml(when) + '</span>' +
                        (m.pipelineTag ? '<span class="badge badge-info">' + escapeHtml(String(m.pipelineTag)) + '</span>' : '') +
                    '</div>' +
                '</div>' +
                '<div class="gguf-result-actions" style="display:flex;gap:6px;margin-top:8px;">' +
                    '<button class="btn btn-sm btn-primary" data-imh-pick="' + escapeHtml(id) + '">' +
                        '<i class="fas fa-list"></i> ' + escapeHtml(t('imageModels.hf_pick_files', 'Показать файлы')) +
                    '</button>' +
                '</div>' +
            '</div>';
        }).join('');
        host.innerHTML = '<div class="gguf-results-toolbar" style="display:flex;justify-content:space-between;align-items:center;margin-bottom:8px;">' +
            '<span class="gguf-results-count" style="font-size:12px;color:var(--text-muted);">' +
                escapeHtml(t('gguf.results_count', 'Found: {n}', { n: list.length })) + '</span>' +
            '</div><div class="gguf-results-list">' + rows + '</div>';
    }

    // =====================================================================
    // Рендер: файлы репозитория + сборка bundle
    // =====================================================================

    function roleOptions(selected) {
        return IMAGE_ROLES.map(function (role) {
            return '<option value="' + role + '"' + (role === selected ? ' selected' : '') + '>' +
                escapeHtml(t('imageModels.role_' + role, role)) + '</option>';
        }).join('');
    }

    /**
     * DIT_FAMILIES — семейства, которые движок грузит ТОЛЬКО через
     * --diffusion-model (pkg/types/image_model.go: diTFamilies). Держим список
     * здесь ещё и для UI-подсказки: если пред-проверка говорит «DiT», а в
     * профиле выбрано семейство all-in-one, движок ответит
     * «get sd version from file failed» — ровно та ошибка, которую мы ловим.
     */
    var DIT_FAMILIES = ['sd3', 'flux', 'flux2', 'chroma', 'qwen_image', 'z_image'];

    /**
     * repoCompatibility — подсказка по автору репозитория (без сети).
     *
     * ЧЕГО ЗДЕСЬ СОЗНАТЕЛЬНО НЕТ: приговора «ComfyUI-сборку sd.cpp не читает».
     * Проверено на движке pinned master-929-3f8527a: экспорт для ComfyUI
     * (раздельные img_mlp.gate_layer + img_mlp.proj) читается наравне со сборкой
     * leejet (fused img_mlp.gate_up) — qwen_image_2_1.hpp:38-43 поддерживает обе
     * раскладки. Поэтому автор — только слабая подсказка, а вердикт даёт
     * пред-проверка заголовка (кнопка «Проверить»).
     */
    function repoCompatibility(repo) {
        var id = String((repo && (repo.id || repo.modelId || repo.name)) || '').toLowerCase();
        if (!id) {
            return {
                level: 'unknown',
                label: t('imageModels.compat_unknown', 'формат не проверен'),
                hint: ''
            };
        }
        var sdCppAuthors = ['leejet/', 'quantstack/', 'silveroxides/'];
        for (var i = 0; i < sdCppAuthors.length; i++) {
            if (id.indexOf(sdCppAuthors[i]) === 0) {
                return {
                    level: 'ok',
                    label: t('imageModels.compat_ok', 'автор публикует сборки под sd.cpp'),
                    hint: t('imageModels.compat_ok_hint', 'Этот автор публикует сборки для stable-diffusion.cpp. Точный ответ всё равно даёт кнопка «Проверить» у файла: она читает заголовок.')
                };
            }
        }
        return {
            level: 'unknown',
            label: t('imageModels.compat_unknown', 'формат не проверен'),
            hint: t('imageModels.compat_unknown_hint', 'Нажмите «Проверить» у файла: воркер прочитает заголовок (без скачивания) и скажет, какое семейство в нём узнаёт движок.')
        };
    }

    /**
     * probeBadge — вердикт пред-проверки заголовка.
     *
     * supported → движок узнаёт семейство (подпись: какое именно) + предупреждение
     * про --diffusion-model для DiT; unknown → по заголовку не определить
     * (VAE/text encoder/LoRA или незнакомое семейство). Приговоров нет.
     */
    function probeBadge(probe) {
        probe = probe || {};
        if (probe.verdict === 'supported' && probe.versionLabel) {
            var hint = probe.reason || '';
            if (probe.dit) {
                hint += ' ' + t('imageModels.probe_dit_hint',
                    'Семейство DiT: в профиле bundle должно быть выбрано это же семейство, тогда воркер запустит движок с --diffusion-model.');
            }
            return {
                level: 'ok',
                label: t('imageModels.probe_ok_family', 'движок узнаёт: {version}', { version: probe.versionLabel }),
                hint: hint
            };
        }
        return {
            level: 'unknown',
            label: t('imageModels.probe_unknown', 'версия по заголовку не определяется'),
            hint: probe.reason || ''
        };
    }

    /** diTFamilyWarning — DiT-файл против выбранного семейства профиля. */
    function diTFamilyWarning(probe, selectedFamily) {
        if (!probe || probe.verdict !== 'supported' || !probe.dit) return '';
        var fam = String(selectedFamily || '');
        // Семейство уже DiT-шное (значит движок получит --diffusion-model) или
        // совпадает с определённым по файлу (Wan/PixArt мапятся в «other») —
        // предупреждать не о чем.
        if (!fam || DIT_FAMILIES.indexOf(fam) !== -1 || fam === probe.family) return '';
        return t('imageModels.probe_family_mismatch',
            'Файл — DiT-семейства ({version}), а в профиле выбрано «{family}»: воркер подключит его как all-in-one (--model) и движок ответит «get sd version from file failed». Выберите семейство {suggested}.',
            { version: probe.versionLabel || '', family: fam, suggested: probe.family || 'DiT' });
    }

    /**
     * ditHint — предупреждение о неполном наборе файлов для DiT-семейств.
     *
     * FLUX/SD3/Qwen-Image/Z-Image/Chroma в sd.cpp требуют отдельные VAE и
     * text encoder; если в репозитории только diffusion-файл, bundle не поедет.
     * Это видно по именам файлов ещё до скачивания.
     */
    function ditHint(files, repoId) {
        var id = String(repoId || '').toLowerCase();
        var fam = '';
        ['flux', 'sd3', 'qwen', 'z-image', 'z_image', 'chroma'].forEach(function (f) {
            if (!fam && id.indexOf(f) !== -1) fam = f;
        });
        if (!fam) return '';
        var roles = {};
        (files || []).forEach(function (f) { roles[suggestedRole(f) || 'diffusion'] = true; });
        if (roles.vae && (roles.clip_l || roles.clip_g || roles.t5xxl || roles.llm)) return '';
        var missing = [];
        if (!roles.vae) missing.push('VAE');
        if (!roles.clip_l && !roles.clip_g && !roles.t5xxl && !roles.llm) missing.push('text encoder');
        return t('imageModels.dit_missing',
            'Похоже на семейство {family}: в sd.cpp для него нужны отдельные {missing} — в этом репозитории их нет. Добавьте их файлами из другого репозитория (например, VAE ae.safetensors).',
            { family: fam, missing: missing.join(' + ') });
    }

    function compatBadgeHtml(badge, probePath) {
        badge = badge || {};
        var color = badge.level === 'ok' ? 'var(--success, #5cb85c)'
            : (badge.level === 'warn' ? 'var(--warning, #e0a030)' : 'var(--text-muted)');
        var icon = badge.level === 'ok' ? 'fa-circle-check' : (badge.level === 'warn' ? 'fa-triangle-exclamation' : 'fa-circle-question');
        var html = '<span class="imh-compat" title="' + escapeHtml(badge.hint || '') + '" style="color:' + color + ';font-size:11px;white-space:nowrap;">' +
            '<i class="fas ' + icon + '"></i> ' + escapeHtml(badge.label || '') + '</span>';
        if (probePath) {
            html += ' <button class="btn btn-secondary btn-sm" data-imh-probe="' + escapeHtml(probePath) + '" ' +
                'title="' + escapeHtml(t('imageModels.probe_hint', 'Прочитать заголовок файла (без скачивания) и проверить, прочитает ли его движок')) + '">' +
                escapeHtml(t('imageModels.probe_btn', 'Проверить')) + '</button>';
        }
        return html;
    }

    function renderFiles() {
        var card = el('imHfFilesCard');
        var body = el('imHfFilesBody');
        if (!card || !body) return;
        if (!state.selectedRepo) { card.style.display = 'none'; return; }
        card.style.display = '';
        var title = el('imHfRepoName');
        if (title) title.textContent = state.selectedRepo;

        if (state.filesLoading) {
            body.innerHTML = '<tr><td colspan="5" class="loading-cell">' +
                escapeHtml(t('common.loading', 'Loading...')) + '</td></tr>';
            return;
        }
        if (state.filesError) {
            body.innerHTML = '<tr><td colspan="5" class="loading-cell" style="color:var(--danger);">' +
                escapeHtml(state.filesError) + '</td></tr>';
            return;
        }
        var files = Array.isArray(state.files) ? state.files : [];
        if (!files.length) {
            body.innerHTML = '<tr><td colspan="5" class="loading-cell">' +
                escapeHtml(t('gguf.no_results', 'Nothing found')) + '</td></tr>';
            return;
        }
        // Предупреждение о неполном наборе (DiT-семейства), вердикт репозитория и
        // расхождение «DiT-файл, а в профиле all-in-one семейство».
        var hint = ditHint(files, state.selectedRepo);
        var repoBadge = repoCompatibility({ id: state.selectedRepo });
        var mainProbe = mainProbeResult(files);
        var header = '';
        var mismatch = diTFamilyWarning(mainProbe, state.bundleFamily);
        if (mismatch) {
            header += '<div class="gguf-local-card" style="margin-bottom:8px;border-left:3px solid var(--warning, #e0a030);">' +
                '<div class="gguf-local-info"><div class="gguf-local-meta" style="white-space:normal;">' +
                '<i class="fas fa-triangle-exclamation"></i> ' + escapeHtml(mismatch) + '</div></div></div>';
        }
        if (hint) {
            header += '<div class="gguf-local-card" style="margin-bottom:8px;border-left:3px solid var(--warning, #e0a030);">' +
                '<div class="gguf-local-info"><div class="gguf-local-meta" style="white-space:normal;">' +
                '<i class="fas fa-triangle-exclamation"></i> ' + escapeHtml(hint) + '</div></div></div>';
        }
        header += '<div style="margin-bottom:8px;font-size:11px;color:var(--text-muted);">' +
            escapeHtml(t('imageModels.repo_verdict', 'Оценка репозитория')) + ': ' + compatBadgeHtml(repoBadge, '') + '</div>';
        if (mainProbe && mainProbe.verdict === 'supported') {
            header += '<div style="margin-bottom:8px;font-size:11px;color:var(--text-muted);">' +
                escapeHtml(t('imageModels.main_file_verdict', 'Главный файл')) + ': ' + compatBadgeHtml(probeBadge(mainProbe), '') + '</div>';
        }
        if (state.familyNotice) {
            header += '<div class="gguf-local-card" style="margin-bottom:8px;border-left:3px solid var(--info, #4a90d9);">' +
                '<div class="gguf-local-info"><div class="gguf-local-meta" style="white-space:normal;">' +
                '<i class="fas fa-circle-info"></i> ' + escapeHtml(state.familyNotice) + '</div></div></div>';
        }

        header += files.map(function (f, idx) {
            var path = filePath(f);
            var sel = state.selection[path] || { checked: false, role: suggestedRole(f) || 'diffusion' };
            var role = sel.role;
            var suggested = suggestedRole(f);
            // Расхождение роли и подсказки сервера показываем как есть: это
            // сигнал «оператор выбрал другое», а не ошибка.
            var roleHint = suggested && suggested !== role
                ? ' <span style="font-size:11px;color:var(--text-muted);">(' + escapeHtml(t('imageModels.hf_suggested', 'предложено')) + ': ' + escapeHtml(suggested) + ')</span>'
                : '';
            // Метка совместимости: результат пред-проверки, если он есть; иначе —
            // «не проверено» + кнопка «Проверить» (для файлов весов).
            var isWeight = /\.(gguf|safetensors|sft|ckpt)$/i.test(path);
            var probe = state.probes[path];
            var compat = '';
            if (probe) {
                compat = compatBadgeHtml(probeBadge(probe), isWeight ? path : '');
            } else if (state.probing[path]) {
                compat = '<span style="font-size:11px;color:var(--text-muted);"><i class="fas fa-spinner fa-spin"></i> ' +
                    escapeHtml(t('imageModels.probe_running', 'проверяю заголовок…')) + '</span>';
            } else if (isWeight) {
                compat = compatBadgeHtml({ level: 'unknown', label: t('imageModels.probe_not_checked', 'не проверено'), hint: '' }, path);
            }
            return '<div class="gguf-file-item" data-imh-file="' + escapeHtml(path) + '">' +
                '<label class="gguf-file-name" style="display:flex;align-items:center;gap:8px;cursor:pointer;">' +
                    '<input type="checkbox" data-imh-check="' + escapeHtml(path) + '"' + (sel.checked ? ' checked' : '') + '>' +
                    '<span style="font-family:monospace;font-size:12px;word-break:break-all;">' + escapeHtml(path) + '</span>' +
                '</label>' +
                '<select class="form-control" data-imh-role="' + escapeHtml(path) + '" style="max-width:200px;font-size:12px;">' +
                    roleOptions(role) +
                '</select>' + roleHint +
                compat +
                '<span class="gguf-file-size">' + escapeHtml(formatBytes(fileSize(f))) + '</span>' +
            '</div>';
        }).join('');
        body.innerHTML = header;
        renderSelectionSummary();
    }

    function selectedCount() {
        return Object.keys(state.selection || {}).filter(function (p) {
            return state.selection[p] && state.selection[p].checked;
        }).length;
    }

    function renderSelectionSummary() {
        var node = el('imHfSelectionSummary');
        if (!node) return;
        var n = selectedCount();
        var total = 0;
        Object.keys(state.selection || {}).forEach(function (p) {
            if (!state.selection[p] || !state.selection[p].checked) return;
            var f = (state.files || []).filter(function (x) { return filePath(x) === p; })[0];
            total += fileSize(f);
        });
        node.textContent = t('imageModels.hf_selected_count', 'Selected files: {n}', { n: n }) +
            (n ? ' · ' + formatBytes(total) : '');
    }

    function renderBundleProgress() {
        var wrap = el('imgBundleProgress');
        if (!wrap) return;
        var p = state.bundle;
        if (!p) { wrap.style.display = 'none'; return; }
        wrap.style.display = '';
        var pct = progressPercent(p);
        var label = el('imgBundleProgressLabel');
        if (label) {
            label.textContent = String(p.bundleId || '') + ': ' +
                t(bundleStatusKey(p.status), p.status || '') + ' - ' + pct + '% (' +
                formatBytes(p.downloaded) + (p.totalBytes ? ' / ' + formatBytes(p.totalBytes) : '') + ')' +
                (p.speedBps ? ' · ' + formatBytes(p.speedBps) + '/s' : '');
        }
        var fill = el('imgBundleProgressFill');
        if (fill) fill.style.width = pct + '%';
        var status = el('imgBundleStatus');
        if (status) {
            var lines = (p.files || []).map(function (f) {
                var fpct = progressPercent(f);
                return '<div>' + escapeHtml(String(f.role || '') + ': ' + (f.filename || f.sourcePath || '')) + ' - ' +
                    escapeHtml(t(bundleStatusKey(f.status), f.status || '')) +
                    (f.status === 'downloading' ? ' ' + fpct + '%' : '') +
                    (f.downloaded ? ' · ' + formatBytes(f.downloaded) : '') +
                    (f.error ? ' · ' + escapeHtml(f.error) : '') + '</div>';
            });
            status.innerHTML = lines.join('');
        }
    }

    // =====================================================================
    // Рендер: «Загрузки»
    // =====================================================================

    function renderProgressBar(pct) {
        return '<div class="gguf-progress-bar" style="height:6px;background:var(--bg-tertiary,#333);border-radius:3px;overflow:hidden;margin-top:6px;">' +
            '<div class="gguf-progress-fill" style="height:100%;width:' + Math.max(0, Math.min(100, Number(pct) || 0)) + '%;background:var(--success);"></div>' +
            '</div>';
    }

    function renderBundleItem(b, isActive) {
        b = b || {};
        var pct = progressPercent(b);
        var files = Array.isArray(b.files) ? b.files : [];
        var filesLine = files.map(function (f) {
            return escapeHtml(String(f.role || '') + ': ' + (f.filename || '') + ' - ' + t(bundleStatusKey(f.status), f.status || ''));
        }).join('<br>');
        return '<div class="gguf-download-item" data-imh-bundle="' + escapeHtml(String(b.bundleId || '')) + '">' +
            '<div class="dl-header" style="display:flex;justify-content:space-between;align-items:center;gap:8px;">' +
                '<span class="dl-name"><i class="fas fa-layer-group"></i> ' + escapeHtml(String(b.bundleId || '')) + '</span>' +
                '<span class="badge">' + escapeHtml(t(bundleStatusKey(b.status), b.status || '')) + '</span>' +
            '</div>' +
            '<div style="font-size:12px;color:var(--text-muted);margin-top:4px;">' +
                pct + '% · ' + escapeHtml(formatBytes(b.downloaded)) + (b.totalBytes ? ' / ' + escapeHtml(formatBytes(b.totalBytes)) : '') +
                (b.speedBps ? ' · ' + escapeHtml(formatBytes(b.speedBps)) + '/s' : '') +
                (b.currentFile ? ' · ' + escapeHtml(String(b.currentFile)) : '') +
                (b.errorMessage ? ' · <span style="color:var(--danger);">' + escapeHtml(String(b.errorMessage)) + '</span>' : '') +
            '</div>' +
            renderProgressBar(pct) +
            (filesLine ? '<div style="font-size:11px;color:var(--text-muted);margin-top:6px;">' + filesLine + '</div>' : '') +
            '<div class="dl-actions" style="display:flex;gap:6px;margin-top:8px;">' +
                (isActive
                    ? '<button class="btn btn-sm btn-secondary" data-imh-cancel="' + escapeHtml(String(b.bundleId || '')) + '">' +
                        '<i class="fas fa-ban"></i> ' + escapeHtml(t('gguf.cancel_download', 'Cancel')) + '</button>'
                    : (b.registered
                        ? '<span class="badge badge-success" title="' + escapeHtml(t('imageModels.bundle_registered', 'Bundle registered')) + '">' +
                            '<i class="fas fa-circle-check"></i> ' + escapeHtml(t('imageModels.bundle_registered', 'Bundle registered')) + '</span>'
                        : '')) +
            '</div>' +
        '</div>';
    }

    function renderFileItem(p, isActive) {
        p = p || {};
        var pct = progressPercent(p);
        return '<div class="gguf-download-item">' +
            '<div class="dl-header" style="display:flex;justify-content:space-between;align-items:center;gap:8px;">' +
                '<span class="dl-name"><i class="fas fa-file"></i> ' + escapeHtml(String(p.filename || '')) + '</span>' +
                '<span class="badge">' + escapeHtml(t(bundleStatusKey(p.status), p.status || '')) + '</span>' +
            '</div>' +
            '<div style="font-size:12px;color:var(--text-muted);margin-top:4px;">' +
                escapeHtml(String(p.modelId || '')) + ' · ' + pct + '% · ' + escapeHtml(formatBytes(p.downloaded)) +
                (p.totalBytes ? ' / ' + escapeHtml(formatBytes(p.totalBytes)) : '') +
                (p.speedBps ? ' · ' + escapeHtml(formatBytes(p.speedBps)) + '/s' : '') +
                (p.errorMessage ? ' · <span style="color:var(--danger);">' + escapeHtml(String(p.errorMessage)) + '</span>' : '') +
            '</div>' +
            renderProgressBar(pct) +
            (isActive ? '<div class="dl-actions" style="margin-top:8px;">' +
                '<button class="btn btn-sm btn-secondary" data-imh-cancel-file="1" data-imh-model="' + escapeHtml(String(p.modelId || '')) + '" data-imh-filename="' + escapeHtml(String(p.filename || '')) + '">' +
                    '<i class="fas fa-ban"></i> ' + escapeHtml(t('gguf.cancel_download', 'Cancel')) + '</button></div>' : '') +
        '</div>';
    }

    function section(titleKey, titleFallback, html, count) {
        if (!html) return '';
        return '<div class="card" style="margin-bottom:12px;">' +
            '<div class="card-header"><h3>' + escapeHtml(t(titleKey, titleFallback)) +
                (count !== undefined ? ' <span class="badge">' + count + '</span>' : '') + '</h3></div>' +
            '<div class="card-body">' + html + '</div></div>';
    }

    function renderDownloads() {
        var host = el('imDownloadsHost');
        if (!host) return;
        if (state.downloadsError) {
            host.innerHTML = '<div class="gguf-empty-state" style="color:var(--danger);">' +
                escapeHtml(state.downloadsError) + '</div>';
            return;
        }
        var snap = downloadsSummary(state.downloads);
        if (!snap.hasAny) {
            host.innerHTML = '<div class="gguf-empty-state">' +
                '<i class="fas fa-cloud-arrow-down" style="opacity:0.4;"></i>' +
                '<div style="margin-top:6px;">' + escapeHtml(t('imageModels.no_downloads', 'Загрузок нет')) + '</div>' +
                '<div style="margin-top:4px;color:var(--text-muted);font-size:12px;">' +
                escapeHtml(t('imageModels.no_downloads_hint', 'Запустите загрузку на табе HuggingFace — прогресс появится здесь.')) +
                '</div></div>';
            return;
        }
        var html = '';
        html += section('gguf.active_downloads', 'Active downloads',
            snap.bundles.map(function (b) { return renderBundleItem(b, true); }).join(''), snap.bundles.length || undefined);
        html += section('imageModels.dl_files_active', 'Active file downloads',
            snap.active.map(function (p) { return renderFileItem(p, true); }).join(''), snap.active.length || undefined);
        html += section('gguf.download_history', 'Download history',
            snap.bundleHistory.map(function (b) { return renderBundleItem(b, false); }).join('') +
            snap.history.map(function (p) { return renderFileItem(p, false); }).join(''),
            (snap.bundleHistory.length + snap.history.length) || undefined);
        if (snap.orphans.length) {
            var orphans = snap.orphans.map(function (o) {
                o = o || {};
                return '<div class="gguf-download-item">' +
                    '<div class="dl-header" style="display:flex;justify-content:space-between;align-items:center;gap:8px;">' +
                        '<span class="dl-name"><i class="fas fa-trash-can"></i> ' + escapeHtml(String(o.filename || o.path || '')) + '</span>' +
                        '<span style="font-size:12px;color:var(--text-muted);">' + escapeHtml(formatBytes(o.size)) + '</span>' +
                    '</div>' +
                    '<div style="font-size:11px;color:var(--text-muted);margin-top:4px;word-break:break-all;">' + escapeHtml(String(o.path || '')) + '</div>' +
                    '<div class="dl-actions" style="margin-top:8px;">' +
                        '<button class="btn btn-sm btn-danger" data-imh-orphan="' + escapeHtml(String(o.filename || '')) + '">' +
                            '<i class="fas fa-trash"></i> ' + escapeHtml(t('gguf.delete', 'Delete')) + '</button>' +
                    '</div>' +
                '</div>';
            }).join('');
            html += section('imageModels.orphans_title', 'Остаточные файлы',
                '<small style="display:block;margin-bottom:8px;color:var(--text-muted);font-size:11px;">' +
                    escapeHtml(t('gguf.orphans_hint', 'Partial .download files from interrupted downloads.')) + '</small>' +
                orphans, snap.orphans.length);
        }
        host.innerHTML = html;
    }

    // =====================================================================
    // Действия
    // =====================================================================

    function requireBackend() {
        if (!backendId()) {
            toast(t('image.no_backend_selected', 'Select an image backend first'), 'error');
            return false;
        }
        return true;
    }

    async function search(query) {
        if (!requireBackend()) return false;
        var q = String(query === undefined ? (el('imHfQuery') ? el('imHfQuery').value : state.query) : query).trim();
        state.query = q;
        if (q.length < 2 && q.indexOf('/') === -1) {
            state.results = [];
            state.searchError = '';
            renderSearchResults();
            return false;
        }
        var taskSel = el('imHfTask');
        state.task = taskSel ? String(taskSel.value || '') : state.task;
        state.searching = true;
        state.searchError = '';
        renderSearchResults();
        try {
            var url = backendUrl('hf/search?query=' + encodeURIComponent(q) + '&limit=' + SEARCH_LIMIT);
            if (state.task) url += '&task=' + encodeURIComponent(state.task);
            var data = await request(url, { timeoutMs: TIMEOUT_SEARCH_MS });
            state.results = (data && Array.isArray(data.results)) ? data.results : [];
            if (!state.results.length) toast(t('gguf.no_results', 'No results'), 'warn');
        } catch (e) {
            state.results = [];
            state.searchError = (e && e.message) || String(e);
        }
        state.searching = false;
        renderSearchResults();
        return true;
    }

    async function pickRepo(repo) {
        if (!requireBackend()) return false;
        state.selectedRepo = String(repo || '');
        state.files = [];
        state.filesError = '';
        state.filesLoading = true;
        state.selection = {};
        // Вердикты пред-проверки относятся к КОНКРЕТНОМУ репозиторию: при выборе
        // нового старые метки (и подставленное семейство) сбрасываем — иначе UI
        // показывал бы чужие имена тензоров как свои.
        state.probes = {};
        state.familyNotice = '';
        renderSearchResults();
        renderFiles();
        try {
            var data = await request(backendUrl('hf/files?modelId=' + encodeURIComponent(state.selectedRepo) +
                '&revision=main'), { timeoutMs: TIMEOUT_SEARCH_MS });
            state.files = (data && Array.isArray(data.files)) ? data.files : [];
            // Отметки по умолчанию — по СЕРВЕРНЫМ ролям (suggestedRole).
            state.selection = defaultSelection(state.files, null);
            // Имя bundle по умолчанию: имя репозитория (оператор поправит).
            var nameInput = el('imgBundleName');
            if (nameInput && !nameInput.value) {
                nameInput.value = String(state.selectedRepo.split('/').pop() || '').toLowerCase()
                    .replace(/[^a-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 64);
            }
        } catch (e) {
            state.filesError = (e && e.message) || String(e);
        }
        state.filesLoading = false;
        renderFiles();
        // Пред-проверка главного файла: вердикт по репозиторию сразу, без кликов.
        autoProbeMainFile();
        return true;
    }

    /**
     * probeFile — пред-проверка файла: читает ТОЛЬКО заголовок (Range-запрос на
     * стороне воркера) и говорит, КАК движок увидит этот файл.
     *
     * ЗАЧЕМ: оператор скачал 4 ГБ GGUF и получил «get sd version from file
     * failed». Причина в семействе, а не в файле: DiT-модель, подключённая как
     * all-in-one (--model), движком не узнаётся. Теперь вердикт («движок узнаёт:
     * Qwen Image 2.1», DiT) видно рядом с файлом ещё до загрузки, а семейство
     * профиля подставляется автоматически (applyProbedFamily).
     */
    async function probeFile(path) {
        if (!path || !backendId()) return null;
        if (state.probing[path]) return state.probes[path] || null;
        state.probing[path] = true;
        renderFiles();
        try {
            var url = backendUrl('hf/probe?modelId=' + encodeURIComponent(state.selectedRepo) +
                '&filename=' + encodeURIComponent(path) + '&revision=main');
            var res = await request(url, { timeoutMs: TIMEOUT_SEARCH_MS });
            state.probes[path] = res || { verdict: 'unknown' };
        } catch (e) {
            state.probes[path] = { verdict: 'unknown', reason: (e && e.message) || String(e) };
        }
        delete state.probing[path];
        // DiT-файл + семейство all-in-one в профиле = «get sd version from file
        // failed» на загрузке. Если оператор не выбирал семейство сам — правим.
        var notice = applyProbedFamily(state.probes[path]);
        if (notice) state.familyNotice = notice;
        renderFiles();
        return state.probes[path];
    }

    /**
     * autoProbeMainFile — после выбора репозитория проверить ГЛАВНЫЙ файл
     * (самый крупный .gguf/.safetensors): один запрос, зато вердикт по репозиторию
     * появляется сразу, без кликов. Остальные файлы — по кнопке.
     */
    async function autoProbeMainFile() {
        var files = (state.files || []).filter(function (f) {
            return /\.(gguf|safetensors)$/i.test(filePath(f));
        });
        if (!files.length) return null;
        files.sort(function (a, b) { return fileSize(b) - fileSize(a); });
        var main = filePath(files[0]);
        if (!main || state.probes[main]) return state.probes[main] || null;
        return probeFile(main);
    }

    function setSelection(path, patch) {
        if (!path) return;
        var cur = state.selection[path] || { checked: false, role: suggestedRole({}), sizeBytes: 0 };
        state.selection[path] = {
            checked: patch && Object.prototype.hasOwnProperty.call(patch, 'checked') ? !!patch.checked : cur.checked,
            role: (patch && patch.role) ? patch.role : cur.role,
            sizeBytes: cur.sizeBytes || 0
        };
        renderSelectionSummary();
    }

    /** Отметить по предложенным сервером ролям (кнопка «как предложено»). */
    function selectBySuggested() {
        state.selection = defaultSelection(state.files, null);
        renderFiles();
        renderSelectionSummary();
        return selectedCount();
    }

    function clearSelection() {
        Object.keys(state.selection).forEach(function (p) { state.selection[p].checked = false; });
        renderFiles();
        renderSelectionSummary();
        return 0;
    }

    function stopPolling() {
        if (state.poll) clearInterval(state.poll);
        state.poll = null;
    }

    function startBundlePolling(name) {
        stopPolling();
        state.pollIdleTicks = 0;
        var tick = async function () {
            if (!backendId() || !name) { stopPolling(); return; }
            try {
                var p = await request(backendUrl('hf/progress?bundleId=' + encodeURIComponent(name)), { timeoutMs: TIMEOUT_CONTROL_MS });
                if (!p) { state.pollIdleTicks++; }
                else {
                    state.pollIdleTicks = 0;
                    state.bundle = p;
                    renderBundleProgress();
                }
            } catch (e) {
                // 404 = записи ещё нет: не ошибка, но и не прогресс.
                state.pollIdleTicks++;
            }
            if (state.pollIdleTicks >= 20) { stopPolling(); return; }
            var status = state.bundle ? String(state.bundle.status || '') : '';
            if (status && status !== 'downloading') {
                stopPolling();
                if (status === 'completed') {
                    toast(t('image.bundle_all_done', 'All bundle files downloaded'), 'success');
                    refreshModels();
                } else {
                    toast(t('image.bundle_failed', 'Bundle download failed') + ': ' + status, 'error');
                }
                refreshDownloads();
            }
        };
        tick();
        state.poll = setInterval(tick, DEFAULT_POLL_MS);
    }

    /** После успешной загрузки список моделей изменился — просим его перечитать. */
    function refreshModels() {
        if (window.ImagePage && window.ImagePage._actions && typeof window.ImagePage._actions.refresh === 'function') {
            try { window.ImagePage._actions.refresh(); } catch (e) { /* страница может быть не смонтирована */ }
        }
    }

    async function startBundle() {
        if (!requireBackend()) return false;
        var nameInput = el('imgBundleName');
        var familySel = el('imgBundleFamily');
        var tokenInput = el('imgHfToken');
        if (tokenInput && tokenInput.value) saveHfToken(tokenInput.value);
        var built = buildFileSpecs(state.selectedRepo, state.selection, {
            name: nameInput ? nameInput.value : ''
        });
        if (built.errors.length) {
            built.errors.forEach(function (err) {
                var info = bundleErrorToI18n(err);
                toast(t(info.key, info.fallback, info.vars), 'error');
            });
            return false;
        }
        var family = familySel && familySel.value ? String(familySel.value) : 'other';
        var body = JSON.stringify({
            name: built.name,
            family: family,
            files: built.files.map(function (f) {
                // sizeBytes нужен воркеру, чтобы не перекачивать уже лежащие
                // файлы нужного размера (hf_bundle.go:128).
                return f.sizeBytes > 0
                    ? { role: f.role, repo: f.repo, filename: f.filename, revision: f.revision, sizeBytes: f.sizeBytes }
                    : { role: f.role, repo: f.repo, filename: f.filename, revision: f.revision };
            })
        });
        state.bundle = { bundleId: built.name, status: 'downloading', downloaded: 0, totalBytes: 0, files: [] };
        renderBundleProgress();
        try {
            await request(backendUrl('hf/bundle'), { method: 'POST', body: body, timeoutMs: TIMEOUT_CONTROL_MS });
        } catch (e) {
            state.bundle = null;
            renderBundleProgress();
            toast(t('image.bundle_failed', 'Bundle download failed') + ': ' + ((e && e.message) || String(e)), 'error');
            return false;
        }
        toast(t('image.bundle_started', 'Bundle download started'), 'success');
        startBundlePolling(built.name);
        // «Загрузки» — тот же модуль: обновим снимок, чтобы активная загрузка
        // была видна сразу, без перезахода на таб.
        refreshDownloads();
        return true;
    }

    async function refreshDownloads() {
        if (!backendId()) {
            state.downloads = null;
            renderDownloads();
            return null;
        }
        try {
            var data = await request(backendUrl('hf/downloads'), { timeoutMs: TIMEOUT_CONTROL_MS });
            state.downloads = data || null;
            state.downloadsError = '';
        } catch (e) {
            state.downloads = null;
            state.downloadsError = (e && e.message) || String(e);
        }
        renderDownloads();
        return state.downloads;
    }

    async function cancelBundle(name) {
        if (!name) return false;
        try {
            await request(backendUrl('hf/cancel'), {
                method: 'POST',
                body: JSON.stringify({ bundleId: name }),
                timeoutMs: TIMEOUT_CONTROL_MS
            });
            toast(t('gguf.download_cancelled', 'Download cancelled'), 'info');
        } catch (e) {
            toast(t('common.error', 'Error') + ': ' + ((e && e.message) || String(e)), 'error');
        }
        stopPolling();
        await refreshDownloads();
        return true;
    }

    async function cancelFile(modelId, filename) {
        try {
            await request(backendUrl('hf/cancel'), {
                method: 'POST',
                body: JSON.stringify({ modelId: modelId, filename: filename }),
                timeoutMs: TIMEOUT_CONTROL_MS
            });
            toast(t('gguf.download_cancelled', 'Download cancelled'), 'info');
        } catch (e) {
            toast(t('common.error', 'Error') + ': ' + ((e && e.message) || String(e)), 'error');
        }
        await refreshDownloads();
        return true;
    }

    /**
     * cleanupOrphan — удаление остаточного .download.
     *
     * DELETE /api/hf/cleanup принимает query-параметры (cmd/sdworker/handlers_hf.go:475),
     * поэтому шлём filename в query: имя может содержать "<bundle>/<file>" — его
     * нельзя класть в путь, только в параметр.
     */
    async function cleanupOrphan(filename) {
        if (!filename) return false;
        try {
            await request(backendUrl('hf/cleanup?filename=' + encodeURIComponent(filename)), {
                method: 'DELETE',
                timeoutMs: TIMEOUT_CONTROL_MS
            });
            toast(t('gguf.file_deleted', 'File deleted'), 'success');
        } catch (e) {
            toast(t('common.error', 'Error') + ': ' + ((e && e.message) || String(e)), 'error');
        }
        await refreshDownloads();
        return true;
    }

    // =====================================================================
    // Монтирование
    // =====================================================================

    function renderFamilySelect() {
        var sel = el('imgBundleFamily');
        if (!sel) return;
        var prev = sel.value || state.bundleFamily;
        sel.innerHTML = IMAGE_FAMILIES.map(function (f) {
            return '<option value="' + escapeHtml(f) + '">' + escapeHtml(f) + '</option>';
        }).join('');
        if (prev) sel.value = prev;
        state.bundleFamily = sel.value || 'sd15';
    }

    function onClick(ev) {
        var target = ev && ev.target;
        while (target && target.getAttribute) {
            var repo = target.getAttribute('data-imh-repo') || target.getAttribute('data-imh-pick');
            if (repo) { pickRepo(repo); return; }
            // Пред-проверка файла (заголовок, без скачивания).
            var probePath = target.getAttribute('data-imh-probe');
            if (probePath) { probeFile(probePath); return; }
            var cancel = target.getAttribute('data-imh-cancel');
            if (cancel) { cancelBundle(cancel); return; }
            var cancelFileBtn = target.getAttribute('data-imh-cancel-file');
            if (cancelFileBtn) {
                cancelFile(target.getAttribute('data-imh-model'), target.getAttribute('data-imh-filename'));
                return;
            }
            var orphan = target.getAttribute('data-imh-orphan');
            if (orphan) { cleanupOrphan(orphan); return; }
            var act = target.getAttribute('data-imh-action');
            if (act === 'search') { search(); return; }
            if (act === 'suggested') { selectBySuggested(); return; }
            if (act === 'clear') { clearSelection(); return; }
            if (act === 'download') { startBundle(); return; }
            if (act === 'refresh-downloads') { refreshDownloads(); return; }
            target = target.parentNode;
        }
    }

    function onChange(ev) {
        var target = ev && ev.target;
        if (!target || !target.getAttribute) return;
        var check = target.getAttribute('data-imh-check');
        if (check !== null && check !== undefined && check !== '') {
            setSelection(check, { checked: !!target.checked });
            return;
        }
        var role = target.getAttribute('data-imh-role');
        if (role) { setSelection(role, { role: target.value }); return; }
        if (target.id === 'imgBundleFamily') {
            state.bundleFamily = target.value;
            // Оператор выбрал семейство руками — автоподстановка больше не лезет.
            state.familyManual = true;
            // Предупреждение «DiT-файл против all-in-one семейства» зависит от
            // этого селекта: без перерисовки оператор увидел бы старую шапку.
            renderFiles();
        }
    }

    /**
     * applyProbedFamily — подставить семейство профиля по заголовку файла.
     *
     * ЗАЧЕМ: именно промах по семейству даёт «get sd version from file failed».
     * DiT-файл (flux/sd3/qwen/z-image/chroma), подключённый как all-in-one
     * (--model), движок не узнаёт; при выборе DiT-семейства воркер сам передаст
     * --diffusion-model. Подставляем ТОЛЬКО если оператор не трогал селект.
     */
    function applyProbedFamily(probe) {
        if (!probe || probe.verdict !== 'supported' || !probe.family) return '';
        if (state.familyManual) return '';
        if (IMAGE_FAMILIES.indexOf(probe.family) === -1) return '';
        if (state.bundleFamily === probe.family) return '';
        var prev = state.bundleFamily;
        state.bundleFamily = probe.family;
        var sel = el('imgBundleFamily');
        if (sel) sel.value = probe.family;
        return t('imageModels.family_autoset',
            'Семейство профиля переключено на «{family}» по заголовку файла (было «{prev}»): иначе движок получит all-in-one флаг и ответит «get sd version from file failed».',
            { family: probe.family, prev: prev });
    }

    /** mainProbeResult — результат пред-проверки главного (самого крупного) файла. */
    function mainProbeResult(files) {
        var list = (files || []).filter(function (f) {
            return /\.(gguf|safetensors)$/i.test(filePath(f)) && state.probes[filePath(f)];
        });
        if (!list.length) return null;
        list.sort(function (a, b) { return fileSize(b) - fileSize(a); });
        return state.probes[filePath(list[0])] || null;
    }

    function mount() {
        if (mounted) return;
        mounted = true;
        var page = el('image-page') || document;
        if (page && page.addEventListener) {
            // Делегирование: контент табов перерисовывается, слушатели — одни.
            page.addEventListener('click', onClick);
            page.addEventListener('change', onChange);
        }
        var query = el('imHfQuery');
        if (query && query.addEventListener) {
            query.addEventListener('keydown', function (ev) {
                if (ev && ev.key === 'Enter') { ev.preventDefault(); search(); }
            });
        }
        renderFamilySelect();
        var tokenInput = el('imgHfToken');
        if (tokenInput && !tokenInput.value) tokenInput.value = hfToken();
    }

    /** render(ctx) — вызывается шеллом при показе таба. */
    function render(ctx) {
        if (ctx) state.ctx = ctx;
        mount();
        var tab = state.ctx && state.ctx.tab;
        if (tab === 'downloads') {
            // Всегда перечитываем снимок: на этом табе данные меняются каждую
            // секунду, а показать прошлый снимок «как есть» — значит соврать
            // (например, показать «Загрузок нет» сразу после старта bundle).
            // Прошлый снимок рисуем сразу, чтобы не мигало пустое место.
            if (state.downloads) renderDownloads();
            refreshDownloads();
            return;
        }
        if (tab === 'hf') {
            renderSearchResults();
            renderFiles();
            renderBundleProgress();
        }
    }

    window.ImageModelsHf = {
        mount: mount,
        render: render,
        tabIds: TAB_IDS,
        _actions: {
            search: search,
            pickRepo: pickRepo,
            setSelection: setSelection,
            selectBySuggested: selectBySuggested,
            clearSelection: clearSelection,
            startBundle: startBundle,
            refreshDownloads: refreshDownloads,
            cancelBundle: cancelBundle,
            cancelFile: cancelFile,
            cleanupOrphan: cleanupOrphan,
            stopPolling: stopPolling,
            saveHfToken: saveHfToken,
            probeFile: probeFile,
            autoProbeMainFile: autoProbeMainFile
        },
        _state: state,
        pure: {
            escapeHtml: escapeHtml,
            formatBytes: formatBytes,
            filePath: filePath,
            fileSize: fileSize,
            suggestedRole: suggestedRole,
            isValidRole: isValidRole,
            defaultSelection: defaultSelection,
            buildFileSpecs: buildFileSpecs,
            bundleErrorToI18n: bundleErrorToI18n,
            progressPercent: progressPercent,
            bundleStatusKey: bundleStatusKey,
            downloadsSummary: downloadsSummary,
            IMAGE_ROLES: IMAGE_ROLES,
            IMAGE_FAMILIES: IMAGE_FAMILIES,
            AUTO_ROLES: AUTO_ROLES,
            repoCompatibility: repoCompatibility,
            probeBadge: probeBadge,
            diTFamilyWarning: diTFamilyWarning,
            ditHint: ditHint,
            compatBadgeHtml: compatBadgeHtml,
            DIT_FAMILIES: DIT_FAMILIES
        }
    };
})();
