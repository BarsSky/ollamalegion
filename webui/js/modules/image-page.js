/**
 * image-page.js — R-Image Phase 9 (2026-10-03): страница «Image-модели».
 *
 * Что делает:
 *   1. Выбор image-бэкенда и модели в шапке страницы.
 *   2. Управление image-моделями (bundle): список, load / unload, прогресс загрузки.
 *   3. Загрузка bundle с HuggingFace (несколько файлов: diffusion + vae + TE).
 *   4. Монтирование редактора профилей (window.ImageProfiles.mount).
 *
 * ПОЧЕМУ БОЛЬШЕ НЕТ ГЕНЕРАЦИИ, РЕЗУЛЬТАТА И ГАЛЕРЕИ (Phase 9):
 *   WebUI — панель настройки балансера и понимания состояния системы: какие
 *   image-бэкенды есть, какие модели лежат на диске, что загружено, как идёт
 *   скачивание с HF. Показ сгенерированных картинок — задача клиентов
 *   (OpenAI/A1111 на :18079): у клиента есть и превью, и своя история, и правка
 *   результата. Дублирование этого в WebUI тянуло за собой форму генерации,
 *   разбор ответа воркера, localStorage-историю с бюджетом в 5 МБ и галерею —
 *   то есть вторую страницу, которая повторяла клиента и для настройки была не
 *   нужна. Проверка «движок отвечает» осталась отдельной кнопкой «Проверка
 *   бэкенда» (image-models-page.js): она шлёт 1 шаг 64x64 и НЕ рисует картинку.
 *
 * ПОЧЕМУ свой мини-клиент, а не GgufApi.requestViaBackend:
 *   - путь прокси другой: /api/v1/image/backends/{id}/proxy (у GGUF —
 *     /api/v1/gguf/backends/{id}/proxy);
 *   - HF-путям нужен заголовок X-HF-Token, а ветку GGUF-клиента трогать нельзя.
 *   Контрольные вызовы ограничены 15 с: загрузку модели и скачивание файлов
 *   выполняет воркер фоном, UI только запускает их и опрашивает прогресс.
 *
 * Зависимости (уже загружены, см. webui/index.html): window.I18N, window.Api,
 * window.GgufApi (только ради общего HF-токена), window.ImageProfiles
 * (редактор профилей, грузится до этого файла).
 * Экспорт: window.ImagePage.
 */
(function () {
    'use strict';

    // ---- Ключи localStorage ----
    // HF-токен общий с GgufApi (gguf-api.js:19) — один токен на WebUI, чтобы
    // оператор не вводил его дважды на двух страницах.
    var HF_TOKEN_KEY = 'ollamalegion_hf_token';

    // ---- Таймауты и интервалы ----
    var TIMEOUT_CONTROL_MS = 15000;
    var LOAD_POLL_MS = 1500;
    var DOWNLOAD_POLL_MS = 2000;
    // Сколько пустых тиков прогресса терпим, прежде чем перестать опрашивать
    // (прогресс может быть недоступен у воркера — не крутим вечный таймер).
    var DOWNLOAD_IDLE_TICKS_MAX = 20;

    // ---- Пути воркера (контракт плана §5.3, проксируются балансером) ----
    var PATH_MODELS = '/api/image/models';
    var PATH_MODEL_LOAD = '/api/image/models/load';
    var PATH_MODEL_UNLOAD = '/api/image/models/unload';
    var PATH_LOAD_PROGRESS = '/api/image/models/load/progress';
    var PATH_HF_FILES = '/api/hf/files';
    var PATH_HF_PROGRESS = '/api/hf/progress';
    var PATH_HF_DOWNLOAD = '/api/hf/download';
    // Bundle-загрузка. Канонический путь — POST /api/hf/bundle на image-воркере:
    // балансер прозрачно форвардит его воркеру и через прокси
    // (/api/v1/image/backends/{id}/proxy/api/hf/bundle), и через алиас
    // /api/v1/image/backends/{id}/pull (internal/api/handlers_image_models.go:225).
    // Тело — {name, family, files:[{role, repo, filename, revision}]}.
    //
    // Дополнительные пути оставлены как страховка от расхождения контракта, а
    // если ни один не отвечает (на момент Phase 5 воркер /api/hf/* ещё не
    // реализует, см. cmd/sdworker/router.go:74-76) — честно деградируем на
    // N одиночных /api/hf/download (он принимает поле role).
    var BUNDLE_DOWNLOAD_PATHS = ['/api/hf/bundle', '/api/hf/bundle/download', '/api/image/models/download'];

    // Роли файлов bundle — ЗАМОРОЖЕННЫЙ контракт pkg/types/image_model.go:32-45.
    var IMAGE_ROLES = ['diffusion', 'vae', 'clip_l', 'clip_g', 't5xxl', 'llm', 'clip_vision', 'taesd', 'lora', 'upscaler', 'controlnet', 'ip_adapter'];
    // Семейства — pkg/types/image_model.go:49-52.
    var IMAGE_FAMILIES = ['sd15', 'sd21', 'sd_turbo', 'sdxl', 'sdxl_turbo', 'sd3', 'flux', 'flux2', 'chroma', 'qwen_image', 'z_image', 'other'];

    // =====================================================================
    // Pure helpers — не трогают DOM, поэтому покрыты юнит-тестами
    // (webui/js/modules/image-page.test.js, запуск: node <файл>).
    // =====================================================================

    /**
     * Экранирование HTML без document.createElement — чтобы модуль можно было
     * прогонять в node без DOM (Utils.escapeHtml требует document).
     */
    function escapeHtml(text) {
        if (text === null || text === undefined) return '';
        return String(text)
            .replace(/&/g, '&amp;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;')
            .replace(/'/g, '&#39;');
    }

    /**
     * Байты → человекочитаемо. 1024-базная шкала (как Utils.formatMB), но с
     * полным набором единиц: размеры image-бандлов от 300 МБ до 12 ГБ.
     */
    function formatBytes(n) {
        if (n === null || n === undefined || n === '' || isNaN(Number(n))) return '-';
        var v = Number(n);
        if (v <= 0) return '0 B';
        var units = ['B', 'KB', 'MB', 'GB', 'TB'];
        var i = 0;
        while (v >= 1024 && i < units.length - 1) { v = v / 1024; i++; }
        return (i === 0 ? String(Math.round(v)) : v.toFixed(v >= 100 ? 0 : 1)) + ' ' + units[i];
    }

    /** Длительность: 12s / 1m 05s / 1h 02m. */
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

    /**
     * Разбор ошибки: OpenAI-конверт {error:{message,type,code}} (его отдаёт
     * ImageRouter для /v1/*, image_router.go:144-153), плоский
     * {error,message} (остальные пути) либо сырой текст/HTML.
     */
    function parseOpenAIError(body, status) {
        var fallback = 'HTTP ' + (status || '?');
        if (body === null || body === undefined || body === '') return fallback;
        if (typeof body === 'string') {
            var text = body.trim();
            if (!text) return fallback;
            if (text.charAt(0) === '{' || text.charAt(0) === '[') {
                try { return parseOpenAIError(JSON.parse(text), status); } catch (e) { /* не JSON */ }
            }
            return text.length > 400 ? text.slice(0, 400) + '...' : text;
        }
        if (typeof body === 'object') {
            var err = body.error;
            if (typeof err === 'string' && err) return err;
            if (err && typeof err === 'object') {
                var msg = err.message || err.code || '';
                if (msg && err.code && err.message) return msg + ' (' + err.code + ')';
                if (msg) return String(msg);
            }
            if (body.message) return String(body.message);
            if (body.detail) return String(body.detail);
        }
        return fallback;
    }

    /** GET /api/image/models → нормализованный список моделей. */
    function normalizeModels(data) {
        var arr = [];
        if (Array.isArray(data)) arr = data;
        else if (data && Array.isArray(data.models)) arr = data.models;
        return arr.map(function (m) {
            m = m || {};
            var d = m.defaults || {};
            return {
                name: m.name || m.id || '',
                state: String(m.state || 'not_loaded'),
                size_bytes: Number(m.size_bytes || m.sizeBytes || 0),
                family: m.family || '',
                active_queries: Number(m.active_queries || m.activeQueries || 0),
                vram_estimate_mb: Number(m.vram_estimate_mb || m.vramEstimateMB || m.vramEstimateMb || 0),
                // Дефолты профиля (steps/cfgScale/sampler/scheduler/width/height/negativePrompt).
                // Форму генерации из WebUI убрали, но сервер их отдаёт, а список
                // моделей - это ещё и обзор состава профиля.
                defaults: d,
                disabled: !!m.disabled
            };
        }).filter(function (m) { return !!m.name; });
    }

    /**
     * Список бэкендов → нормализованный вид.
     * @param {*} data ответ /api/v1/image/backends или /api/v1/backends
     * @param {boolean} onlyImageType true для /api/v1/backends: там лежат ВСЕ
     *        типы, и image-бэкенды надо отфильтровать по type/backendType/engine.
     */
    function normalizeBackends(data, onlyImageType) {
        var arr = [];
        if (Array.isArray(data)) arr = data;
        else if (data && Array.isArray(data.backends)) arr = data.backends;
        return arr.map(function (b) {
            b = b || {};
            var rawType = String(b.type || b.backendType || b.backend_type || b.engine || '').toLowerCase();
            return {
                id: b.id || b.ID || '',
                name: b.name || b.id || '',
                host: b.host || '',
                // imagePort — поле конфига image-бэкенда (Backend.ImagePort,
                // fallback EffectiveImagePort() на сервере); image_port — на случай
                // snake_case-сериализации в чужих хендлерах.
                imagePort: Number(b.imagePort || b.image_port || 0),
                status: b.status || '',
                type: rawType
            };
        }).filter(function (b) {
            if (!b.id) return false;
            if (!onlyImageType) return true;
            return b.type === 'image_cpp' || b.type === 'sd_cpp' || b.type === 'sdcpp';
        });
    }

    /**
     * Строки формы bundle → файлы профиля. Ошибки возвращаются КОДАМИ
     * (не текстом), чтобы pure-слой не зависел от языка.
     * Пустые строки игнорируются (пользователь мог добавить и не заполнить).
     */
    function parseBundleRows(rows) {
        var files = [];
        var errors = [];
        var seen = Object.create(null);
        (rows || []).forEach(function (r, idx) {
            r = r || {};
            var role = String(r.role || '').trim();
            var repo = String(r.repo || '').trim();
            var filename = String(r.filename || '').trim();
            var rowNo = idx + 1;
            if (!role && !repo && !filename) return;
            if (IMAGE_ROLES.indexOf(role) < 0) {
                errors.push({ code: 'role', row: rowNo, role: role });
                return;
            }
            if (!repo || !filename) {
                errors.push({ code: 'missing', row: rowNo });
                return;
            }
            // Дубликат роли запрещён, кроме lora: LoRA-файлов может быть много
            // (pkg/types/image_model.go:251 - "duplicate role" только для остальных).
            if (seen[role] && role !== 'lora') {
                errors.push({ code: 'dup', row: rowNo, role: role });
                return;
            }
            seen[role] = true;
            files.push({
                role: role,
                repo: repo,
                filename: filename,
                revision: String(r.revision || 'main').trim() || 'main'
            });
        });
        if (files.length && !seen.diffusion) {
            errors.push({ code: 'need_diffusion', row: 0 });
        }
        return { files: files, errors: errors };
    }

    /** Ключ файла bundle для карты прогресса. */
    function bundleFileKey(f) {
        return String((f && f.repo) || '') + '/' + String((f && f.filename) || '');
    }

    /**
     * Агрегат прогресса по файлам bundle: суммарные байты, процент,
     * сколько файлов завершено/упало. Прогресс берётся из HFDownloadProgress
     * (downloaded/totalBytes/status, hf_downloader.go:122-143).
     */
    function aggregateDownloadProgress(files, progressMap) {
        var total = 0;
        var downloaded = 0;
        var completed = 0;
        var failed = 0;
        var active = 0;
        var unknown = 0;
        var list = files || [];
        list.forEach(function (f) {
            var p = progressMap ? progressMap[bundleFileKey(f)] : null;
            if (!p) { unknown++; return; }
            var t = Number(p.totalBytes || p.total_bytes || 0);
            var d = Number(p.downloaded || p.downloadedBytes || 0);
            var st = String(p.status || '');
            if (st === 'completed') {
                completed++;
                total += t > 0 ? t : d;
                downloaded += t > 0 ? t : d;
                return;
            }
            if (st === 'failed' || st === 'cancelled' || st === 'interrupted') {
                failed++;
                total += t > 0 ? t : 0;
                return;
            }
            active++;
            total += t > 0 ? t : 0;
            downloaded += d > 0 ? d : 0;
        });
        var pct = 0;
        if (total > 0) pct = Math.floor((downloaded / total) * 100);
        else if (list.length > 0 && completed + failed === list.length && failed === 0) pct = 100;
        if (pct > 100) pct = 100;
        return {
            total: total,
            downloaded: downloaded,
            percent: pct,
            completed: completed,
            failed: failed,
            active: active,
            unknown: unknown,
            totalFiles: list.length,
            done: list.length > 0 && completed + failed === list.length
        };
    }

    /** Состояние модели → ключ i18n (переиспользуем gguf.model_state_*). */
    function stateLabelKey(st) {
        var s = String(st || '').toLowerCase();
        if (s === 'loaded') return 'gguf.model_state_loaded';
        if (s === 'loading') return 'gguf.model_state_loading';
        if (s === 'error' || s === 'failed') return 'gguf.model_state_error';
        return 'gguf.model_state_unloaded';
    }

    /**
     * Нормализация ответа /api/image/models/load/progress к одному снимку.
     *
     * Формы ответа, которые надо поддержать:
     *   - image-воркер (cmd/sdworker/handlers_model.go:318-338):
     *     { progress: {state, model, stage, elapsed_ms, error, events}, state, model, pid };
     *   - зеркало cppworker-контракта: { models: [{name, state, loadingStartedAt, ...}] };
     *   - одиночный объект {name|model, state, error}.
     *
     * Возвращает {state, stage, elapsedMs, error, model} либо null, если запись
     * про запрошенную модель отсутствует (значит, загрузка уже завершилась и
     * трекер её не помнит).
     */
    function normalizeLoadProgress(data, name) {
        var p = data || {};
        var arr = [];
        if (Array.isArray(p)) arr = p;
        else if (Array.isArray(p.models)) arr = p.models;
        else if (p.progress && typeof p.progress === 'object') arr = [p.progress];
        else if (p.name || p.model || p.state) arr = [p];
        if (!arr.length) return null;
        var entry = null;
        for (var i = 0; i < arr.length; i++) {
            var n = arr[i].name || arr[i].model || '';
            if (!name || n === name || n === '') { entry = arr[i]; break; }
        }
        if (!entry) return null;
        return {
            state: String(entry.state || ''),
            stage: String(entry.stage || ''),
            // elapsed_ms — формат image-воркера, elapsedMs — зеркало cppworker.
            elapsedMs: Number(entry.elapsed_ms || entry.elapsedMs || 0),
            // progressPct обычно отсутствует (sd-server процента не отдаёт) —
            // оставляем NaN, чтобы UI не рисовал выдуманную полосу.
            progressPct: entry.progressPct !== undefined ? Number(entry.progressPct) : NaN,
            error: entry.error || '',
            model: entry.name || entry.model || ''
        };
    }

    var pure = {
        escapeHtml: escapeHtml,
        formatBytes: formatBytes,
        formatDuration: formatDuration,
        parseOpenAIError: parseOpenAIError,
        normalizeModels: normalizeModels,
        normalizeBackends: normalizeBackends,
        parseBundleRows: parseBundleRows,
        aggregateDownloadProgress: aggregateDownloadProgress,
        bundleFileKey: bundleFileKey,
        stateLabelKey: stateLabelKey,
        normalizeLoadProgress: normalizeLoadProgress,
        IMAGE_ROLES: IMAGE_ROLES,
        IMAGE_FAMILIES: IMAGE_FAMILIES
    };

    // =====================================================================
    // Состояние страницы
    // =====================================================================

    var state = {
        bound: false,
        inited: false,
        backends: [],
        selectedBackendId: '',
        models: [],
        loadPoll: null,         // { timer, name }
        bundleRows: [],         // строки формы bundle (переживают смену языка)
        bundleFiles: [],
        bundleProgress: {},
        bundlePoll: null,
        bundleIdleTicks: 0,
        notifiedApiMissing: false
    };

    // =====================================================================
    // i18n / toast / DOM хелперы
    // =====================================================================

    function t(key, fallback, vars) {
        if (window.I18N && I18N.t) {
            var s = I18N.t(key, vars);
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
        if (window.console) console.warn('[ImagePage]', msg);
    }

    function el(id) { return document.getElementById(id); }
    function val(id) { var e = el(id); return e ? e.value : ''; }
    function setText(id, text) { var e = el(id); if (e) e.textContent = text; }
    function show(id, visible, displayValue) {
        var e = el(id);
        if (!e) return;
        e.style.display = visible ? (displayValue || '') : 'none';
    }

    // =====================================================================
    // Сеть
    // =====================================================================

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

    /**
     * Заголовки запроса. Базовый набор берём у Api.getAuthHeaders() — это общий
     * хелпер проекта (api.js:651-662, R59.5), чтобы токен и Content-Type не
     * разъезжались между модулями.
     */
    function authHeaders(extra) {
        var headers = (window.Api && typeof window.Api.getAuthHeaders === 'function')
            ? window.Api.getAuthHeaders()
            : { 'Content-Type': 'application/json' };
        if (extra) {
            Object.keys(extra).forEach(function (k) { headers[k] = extra[k]; });
        }
        // Api.getAuthHeaders() читает токен только из WEBUI_CONFIG.API_TOKEN; в dev
        // (без entrypoint.sh) его там нет, поэтому добавляем тот же fallback на
        // localStorage, что и backend-type-filter.js:70-71.
        if (!headers['X-API-Token']) {
            var tk = apiToken();
            if (tk) headers['X-API-Token'] = tk;
        }
        return headers;
    }

    function hfToken() {
        try { return localStorage.getItem(HF_TOKEN_KEY) || ''; } catch (e) { return ''; }
    }

    /**
     * URL прокси к image-воркеру через балансер.
     * Зеркало GgufApi.buildBackendProxyUrl (gguf-api.js:857-876):
     * ?token= дублируем в query, потому что заголовок X-API-Token не умеют
     * EventSource/скачивание по прямой ссылке (AuthMiddleware принимает оба).
     */
    function buildProxyUrl(backendId, path) {
        var url = apiBase() + '/api/v1/image/backends/' + encodeURIComponent(backendId) + '/proxy' + path;
        var tk = apiToken();
        if (tk && url.indexOf('token=') === -1) {
            url += (url.indexOf('?') === -1 ? '?' : '&') + 'token=' + encodeURIComponent(tk);
        }
        return url;
    }

    /** fetch + JSON + разбор конверта ошибки (бросает Error со status/body). */
    function requestJson(url, opts) {
        opts = opts || {};
        var timeoutMs = opts.timeoutMs || TIMEOUT_CONTROL_MS;
        var ctrl = (typeof AbortController !== 'undefined') ? new AbortController() : null;
        var timer = null;
        if (ctrl) {
            timer = setTimeout(function () { ctrl.abort(); }, timeoutMs);
        }
        var init = { method: opts.method || 'GET', headers: authHeaders(opts.headers) };
        if (ctrl) init.signal = ctrl.signal;
        if (opts.hfTokenValue) init.headers['X-HF-Token'] = opts.hfTokenValue;
        if (opts.body !== undefined) init.body = opts.body;
        return fetch(url, init).then(function (resp) {
            if (timer) clearTimeout(timer);
            return resp.text().then(function (text) {
                var parsed = null;
                if (text) {
                    try { parsed = JSON.parse(text); } catch (e) { parsed = text; }
                }
                if (!resp.ok) {
                    var err = new Error(parseOpenAIError(parsed, resp.status));
                    err.status = resp.status;
                    err.body = parsed;
                    if (resp.status === 429 && resp.headers && resp.headers.get) {
                        var ra = resp.headers.get('Retry-After');
                        if (ra) err.message += ' (retry after ' + ra + 's)';
                    }
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

    /** Запрос к управляющей плоскости балансера (18081). */
    function requestControl(path, opts) {
        opts = opts || {};
        return requestJson(apiBase() + path, opts);
    }

    /** Запрос к image-воркеру через прокси балансера. */
    function requestBackend(backendId, path, opts) {
        opts = opts || {};
        var o = {
            method: opts.method,
            body: opts.body,
            timeoutMs: opts.timeoutMs || TIMEOUT_CONTROL_MS,
            headers: opts.headers
        };
        // HF-токен нужен только HF-путям (как в gguf-api.js:1015).
        if (path.indexOf('/api/hf/') === 0) o.hfTokenValue = hfToken();
        return requestJson(buildProxyUrl(backendId, path), o);
    }

    function isMissingEndpoint(err) {
        return !!err && (err.status === 404 || err.status === 405 || err.status === 501);
    }

    // =====================================================================
    // Бэкенды / модели
    // =====================================================================

    function setNotice(html) {
        var box = el('imgNotice');
        if (!box) return;
        if (!html) { box.style.display = 'none'; box.innerHTML = ''; return; }
        box.innerHTML = html;
        box.style.display = '';
    }

    async function loadBackends() {
        var list = [];
        try {
            var data = await requestControl('/api/v1/image/backends', { timeoutMs: TIMEOUT_CONTROL_MS });
            list = normalizeBackends(data, false);
        } catch (e) {
            // Эндпоинт /api/v1/image/backends — часть параллельной бэкенд-работы.
            // Если его ещё нет, не показываем пустую страницу: берём
            // GET /api/v1/backends (он есть всегда) и фильтруем по типу сами.
            if (isMissingEndpoint(e) || e.code === 'timeout') {
                var all = await requestControl('/api/v1/backends', { timeoutMs: TIMEOUT_CONTROL_MS });
                list = normalizeBackends(all, true);
            } else {
                throw e;
            }
        }
        state.backends = list;
        var keep = state.selectedBackendId;
        var stillThere = list.some(function (b) { return b.id === keep; });
        state.selectedBackendId = stillThere ? keep : (list.length ? list[0].id : '');
        renderBackendSelect();
        if (!list.length) {
            setNotice(t('image.no_backends', 'No image backend registered. Add a backend with type image.cpp on the Backends page.'));
        }
        return list;
    }

    async function loadModels() {
        var backendId = state.selectedBackendId;
        state.bundleProgress = {};
        if (!backendId) {
            state.models = [];
            renderModelSelect();
            renderModelsTable();
            return;
        }
        try {
            var data = await requestBackend(backendId, PATH_MODELS, { timeoutMs: TIMEOUT_CONTROL_MS });
            state.models = normalizeModels(data);
            if (!state.backends.length) setNotice(t('image.no_backends', 'No image backend registered.'));
            else setNotice('');
        } catch (e) {
            state.models = [];
            var msg = (e && e.message) || String(e);
            if (isMissingEndpoint(e)) {
                // Воркер ещё не реализовал image-эндпоинты — это ожидаемое
                // состояние на момент Phase 5, поэтому не «ошибка», а подсказка.
                setNotice(escapeHtml(t('image.api_unavailable', 'Image worker API is not available yet ({error}).', { error: msg })));
                if (!state.notifiedApiMissing) {
                    state.notifiedApiMissing = true;
                    toast(t('image.api_unavailable', 'Image worker API is not available yet ({error}).', { error: msg }), 'warn');
                }
            } else {
                setNotice(escapeHtml(t('image.backend_unavailable', 'Image backend is unreachable: {error}', { error: msg })));
            }
        }
        renderModelSelect();
        renderModelsTable();
    }

    /** Полный refresh данных страницы (вызывается при входе и по кнопке). */
    async function refresh() {
        try {
            await loadBackends();
        } catch (e) {
            setNotice(escapeHtml(t('image.backend_unavailable', 'Image backend is unreachable: {error}', { error: (e && e.message) || String(e) })));
            return;
        }
        // Модели грузим после бэкендов: нужен выбранный backendId.
        await loadModels();
    }

    // =====================================================================
    // Рендер: селекты бэкенда/модели
    // =====================================================================

    function renderBackendSelect() {
        var sel = el('imgBackendSelect');
        if (!sel) return;
        if (!state.backends.length) {
            sel.innerHTML = '<option value="">' + escapeHtml(t('image.unknown', 'unknown')) + '</option>';
            return;
        }
        sel.innerHTML = state.backends.map(function (b) {
            var label = b.name || b.id;
            if (b.host) label += ' (' + b.host + (b.imagePort ? ':' + b.imagePort : '') + ')';
            return '<option value="' + escapeHtml(b.id) + '"' + (b.id === state.selectedBackendId ? ' selected' : '') + '>' +
                escapeHtml(label) + '</option>';
        }).join('');
    }

    function renderModelSelect() {
        var sel = el('imgModelSelect');
        if (!sel) return;
        if (!state.models.length) {
            sel.innerHTML = '<option value="">' + escapeHtml(t('image.select_model', 'Select a model')) + '</option>';
            return;
        }
        var current = sel.value;
        sel.innerHTML = state.models.map(function (m) {
            var suffix = (m.state && m.state !== 'not_loaded') ? ' [' + m.state + ']' : '';
            return '<option value="' + escapeHtml(m.name) + '">' + escapeHtml(m.name + suffix) + '</option>';
        }).join('');
        // Сохраняем выбор пользователя, если модель ещё в списке.
        if (current && state.models.some(function (m) { return m.name === current; })) {
            sel.value = current;
        }
    }

    // =====================================================================
    // Рендер: таблица моделей
    // =====================================================================

    function renderModelsTable() {
        var body = el('imgModelsBody');
        if (!body) return;
        if (!state.selectedBackendId) {
            body.innerHTML = '<tr><td colspan="7" class="loading-cell">' + escapeHtml(t('image.no_backend_selected', 'Select an image backend first')) + '</td></tr>';
            return;
        }
        if (!state.models.length) {
            body.innerHTML = '<tr><td colspan="7" class="loading-cell">' + escapeHtml(t('image.no_models', 'No image models on this backend.')) + '</td></tr>';
            return;
        }
        body.innerHTML = state.models.map(function (m) {
            var isLoaded = m.state === 'loaded';
            var isBusy = m.state === 'loading';
            var actions = '';
            if (isBusy) {
                actions = '<span style="font-size:12px;color:var(--text-muted);"><i class="fas fa-circle-notch fa-spin"></i> ' +
                    escapeHtml(t('gguf.loading_indicator', 'Loading')) + '</span>';
            } else if (isLoaded) {
                actions = '<button class="btn btn-secondary btn-sm" data-img-action="unload-model" data-model="' + escapeHtml(m.name) + '">' +
                    escapeHtml(t('gguf.unload_model', 'Unload')) + '</button>';
            } else {
                actions = '<button class="btn btn-primary btn-sm" data-img-action="load-model" data-model="' + escapeHtml(m.name) + '">' +
                    escapeHtml(t('common.load', 'Load')) + '</button>';
            }
            return '<tr>' +
                '<td><strong>' + escapeHtml(m.name) + '</strong>' + (m.disabled ? ' <span class="badge">disabled</span>' : '') + '</td>' +
                '<td>' + escapeHtml(t(stateLabelKey(m.state), m.state)) + '</td>' +
                '<td>' + escapeHtml(formatBytes(m.size_bytes)) + '</td>' +
                '<td>' + escapeHtml(m.family || '-') + '</td>' +
                '<td>' + escapeHtml(String(m.active_queries || 0)) + '</td>' +
                '<td>' + (m.vram_estimate_mb ? escapeHtml(formatBytes(m.vram_estimate_mb * 1024 * 1024)) : '-') + '</td>' +
                '<td>' + actions + '</td>' +
                '</tr>';
        }).join('');
    }

    // =====================================================================
    // Load / Unload модели + прогресс
    // =====================================================================

    async function onLoadModel(name) {
        var backendId = state.selectedBackendId;
        if (!backendId || !name) return;
        try {
            await requestBackend(backendId, PATH_MODEL_LOAD, {
                method: 'POST',
                body: JSON.stringify({ name: name }),
                timeoutMs: TIMEOUT_CONTROL_MS
            });
            toast(t('image.load_started', 'Loading {name}...', { name: name }), 'info');
            startLoadPolling(name);
        } catch (e) {
            toast(t('image.load_failed', 'Failed to load model: {error}', { error: (e && e.message) || String(e) }), 'error');
        }
    }

    async function onUnloadModel(name) {
        var backendId = state.selectedBackendId;
        if (!backendId || !name) return;
        try {
            // ВАЖНО: image-воркер выгружает ТЕКУЩУЮ модель (body {name} он
            // игнорирует, cmd/sdworker/handlers_model.go:284), а во время
            // генерации отвечает 409 "generation in progress" — это осмысленная
            // ошибка, поэтому показываем её текст как есть, а не глотаем.
            await requestBackend(backendId, PATH_MODEL_UNLOAD, {
                method: 'POST',
                body: JSON.stringify({ name: name }),
                timeoutMs: TIMEOUT_CONTROL_MS
            });
            toast(t('image.unload_done', 'Model unloaded: {name}', { name: name }), 'success');
            await loadModels();
        } catch (e) {
            toast(t('image.unload_failed', 'Failed to unload model: {error}', { error: (e && e.message) || String(e) }), 'error');
        }
    }

    /**
     * Прогресс загрузки модели — polling.
     *
     * У воркера есть и SSE (/api/image/models/load/progress/stream,
     * cmd/sdworker/router.go:63), но для страницы, которую открывают на десятки
     * секунд, достаточно опроса: он не держит постоянное соединение через
     * прокси балансера и не требует ?token= в EventSource. Паттерн опроса взят
     * из gguf-load-progress.js:44-74 и gguf-renderer-refresh.js:364-417.
     */
    function startLoadPolling(name) {
        stopLoadPolling();
        var backendId = state.selectedBackendId;
        var tick = function () {
            requestBackend(backendId, PATH_LOAD_PROGRESS, { timeoutMs: TIMEOUT_CONTROL_MS }).then(function (data) {
                var snap = normalizeLoadProgress(data, name);
                if (!snap) {
                    // Трекер прогресса уже не помнит запись — считаем загрузку
                    // завершённой и перечитываем список моделей (истина о state).
                    stopLoadPolling();
                    renderLoadProgress(null);
                    loadModels();
                    return;
                }
                renderLoadProgress(snap);
                var st = snap.state;
                if (st === 'error' || st === 'failed') {
                    stopLoadPolling();
                    toast(t('image.load_failed', 'Failed to load model: {error}', { error: snap.error || t('image.unknown', 'unknown') }), 'error');
                    loadModels();
                } else if (st === 'loaded' || st === 'ready') {
                    stopLoadPolling();
                    toast(t('image.load_done', 'Model loaded: {name}', { name: name }), 'success');
                    loadModels();
                }
                // 'loading'/'starting'/'warming' — продолжаем опрос.
            }).catch(function (err) {
                // 404/недоступность прогресса не должны ломать загрузку:
                // останавливаем опрос и просто перечитываем список моделей.
                stopLoadPolling();
                renderLoadProgress(null);
                if (err && !isMissingEndpoint(err)) loadModels();
            });
        };
        tick();
        state.loadPoll = { timer: setInterval(tick, LOAD_POLL_MS), name: name };
    }

    /**
     * Отрисовка прогресса загрузки модели.
     *
     * ЗАЧЕМ без процента: image-воркер отдаёт снимок
     * {state, stage, elapsed_ms, error} (cmd/sdworker/handlers_model.go:36-44) —
     * процента загрузки 2-12 ГБ sd-server не сообщает. Рисовать «примерно 40%»
     * было бы выдумкой, поэтому показываем стадию и прошедшее время, а полосу
     * прогресса включаем только если воркер реально прислал progressPct
     * (совместимость с зеркалом cppworker-контракта).
     */
    function renderLoadProgress(snap) {
        if (!snap) { show('imgLoadProgress', false); return; }
        show('imgLoadProgress', true);
        var elapsed = snap.elapsedMs ? formatDuration(snap.elapsedMs) : '';
        var label = snap.stage
            ? t('image.load_progress', 'Loading {name}: {stage} ({t})', { name: snap.model || '', stage: snap.stage, t: elapsed })
            : t('image.load_progress_plain', 'Loading {name} ({t})', { name: snap.model || '', t: elapsed });
        setText('imgLoadProgressLabel', label);
        var pct = Number(snap.progressPct);
        var hasPct = !isNaN(pct) && pct > 0;
        show('imgLoadProgressBar', hasPct);
        var fill = el('imgLoadProgressFill');
        if (fill && hasPct) fill.style.width = Math.max(0, Math.min(100, pct)) + '%';
    }
    function stopLoadPolling() {
        if (state.loadPoll && state.loadPoll.timer) clearInterval(state.loadPoll.timer);
        state.loadPoll = null;
    }

    // =====================================================================
    // Bundle (HF)
    // =====================================================================

    /**
     * Отрисовка строк bundle.
     * @param {Array} [rows] если переданы — используются они и запоминаются в
     *        state.bundleRows. Без аргумента берётся state.bundleRows (или одна
     *        пустая строка). Так re-render по смене языка не теряет введённые
     *        значения: вызывающий сам решает, брать ли их из DOM.
     */
    function renderBundleRows(rows) {
        var box = el('imgBundleRows');
        if (!box) return;
        if (rows) state.bundleRows = rows;
        var list = state.bundleRows && state.bundleRows.length ? state.bundleRows : [{ role: 'diffusion', repo: '', filename: '' }];
        state.bundleRows = list;
        box.innerHTML = list.map(function (r, i) {
            var roleOptions = IMAGE_ROLES.map(function (role) {
                return '<option value="' + role + '"' + (role === r.role ? ' selected' : '') + '>' + role + '</option>';
            }).join('');
            return '<div class="form-row" data-bundle-row="' + i + '" style="align-items:flex-end;">' +
                '<div class="form-group" style="max-width:170px;">' +
                    '<label>' + escapeHtml(t('image.bundle_role', 'Role')) + '</label>' +
                    '<select class="form-control" data-bundle-field="role">' + roleOptions + '</select>' +
                '</div>' +
                '<div class="form-group" style="flex:2;">' +
                    '<label>' + escapeHtml(t('image.bundle_repo', 'Repo (org/model)')) + '</label>' +
                    '<input type="text" class="form-control" data-bundle-field="repo" value="' + escapeHtml(r.repo || '') + '" placeholder="leejet/Z-Image-Turbo-GGUF" list="imgBundleFilesList_' + i + '">' +
                    '<datalist id="imgBundleFilesList_' + i + '"></datalist>' +
                '</div>' +
                '<div class="form-group" style="flex:2;">' +
                    '<label>' + escapeHtml(t('image.bundle_filename', 'File in repo')) + '</label>' +
                    '<input type="text" class="form-control" data-bundle-field="filename" value="' + escapeHtml(r.filename || '') + '" placeholder="z-image-turbo-Q3_K.gguf">' +
                '</div>' +
                '<div class="form-group" style="max-width:190px;">' +
                    '<button class="btn btn-secondary btn-sm" data-img-action="bundle-files" data-row="' + i + '">' + escapeHtml(t('image.bundle_list_files', 'List files')) + '</button> ' +
                    '<button class="btn btn-secondary btn-sm" data-img-action="bundle-remove" data-row="' + i + '">' + escapeHtml(t('image.bundle_remove_row', 'Remove row')) + '</button>' +
                '</div>' +
                '</div>';
        }).join('');
    }

    /**
     * Подсказка роли для новой строки: следующая незанятая из типового набора.
     * ЗАЧЕМ: 90% bundle'ов для sd.cpp - это diffusion + vae + text encoder'ы;
     * предлагать в новой строке 'lora' (первый свободный по алфавиту) значит
     * заставлять оператора каждый раз переключать селект вручную.
     */
    function nextSuggestedRole(rows) {
        var order = ['diffusion', 'vae', 'clip_l', 'clip_g', 't5xxl', 'llm', 'clip_vision', 'taesd', 'lora'];
        var used = {};
        (rows || []).forEach(function (r) { used[r.role] = true; });
        for (var i = 0; i < order.length; i++) {
            if (!used[order[i]]) return order[i];
        }
        return 'lora';
    }

    /** Селект семейства: значения - контракт pkg/types/image_model.go (ImageModelFamilies). */
    function renderFamilySelect() {
        var sel = el('imgBundleFamily');
        if (!sel) return;
        var prev = sel.value;
        sel.innerHTML = IMAGE_FAMILIES.map(function (f) {
            return '<option value="' + escapeHtml(f) + '">' + escapeHtml(f) + '</option>';
        }).join('');
        if (prev) sel.value = prev;
    }

    function readBundleRows() {
        var box = el('imgBundleRows');
        if (!box) return [];
        var rows = [];
        box.querySelectorAll('[data-bundle-row]').forEach(function (row) {
            var get = function (field) {
                var inp = row.querySelector('[data-bundle-field="' + field + '"]');
                return inp ? inp.value : '';
            };
            rows.push({ role: get('role'), repo: get('repo'), filename: get('filename'), revision: 'main' });
        });
        return rows;
    }

    async function listBundleFiles(rowIndex) {
        var backendId = state.selectedBackendId;
        var rows = readBundleRows();
        var row = rows[rowIndex];
        if (!backendId || !row || !row.repo) {
            toast(t('image.bundle_row_missing_fields', 'Row {row}: repo and file are required', { row: rowIndex + 1 }), 'error');
            return;
        }
        try {
            var qs = '?modelId=' + encodeURIComponent(row.repo) + '&revision=main';
            var data = await requestBackend(backendId, PATH_HF_FILES + qs, { timeoutMs: TIMEOUT_CONTROL_MS });
            var files = (data && Array.isArray(data.files)) ? data.files : (Array.isArray(data) ? data : []);
            var list = el('imgBundleFilesList_' + rowIndex);
            if (list) {
                list.innerHTML = files.map(function (f) {
                    var name = f.path || f.filename || f.name || '';
                    return '<option value="' + escapeHtml(name) + '">' + escapeHtml(formatBytes(f.sizeBytes || f.size_bytes || 0)) + '</option>';
                }).join('');
            }
            if (!files.length) toast(t('image.bundle_files_empty', 'No files found in this repo'), 'warn');
            else toast(t('image.bundle_files_found', 'Files in repo: {n}', { n: files.length }), 'info');
        } catch (e) {
            toast(t('common.error', 'Error') + ': ' + ((e && e.message) || String(e)), 'error');
        }
    }

    function saveHfToken() {
        var input = el('imgHfToken');
        if (!input) return;
        try {
            if (input.value) localStorage.setItem(HF_TOKEN_KEY, input.value);
            else localStorage.removeItem(HF_TOKEN_KEY);
        } catch (e) { /* ignore */ }
        if (window.GgufApi && typeof window.GgufApi.setHFToken === 'function') {
            window.GgufApi.setHFToken(input.value);
        }
        toast(t('image.hf_token_saved', 'HF token saved'), 'success');
    }

    function renderBundleProgress(agg, files) {
        var wrap = el('imgBundleProgress');
        if (!wrap) return;
        if (!agg || !agg.totalFiles) { wrap.style.display = 'none'; return; }
        wrap.style.display = '';
        setText('imgBundleProgressLabel', t('image.bundle_progress', 'Overall progress') + ': ' +
            t('image.bundle_overall', 'Downloaded {done} of {total} ({pct}%)', {
                done: formatBytes(agg.downloaded),
                total: agg.total > 0 ? formatBytes(agg.total) : '?',
                pct: agg.percent
            }));
        var fill = el('imgBundleProgressFill');
        if (fill) fill.style.width = agg.percent + '%';

        // Пофайловый статус: оператору важно видеть, какой компонент bundle
        // (vae / t5xxl) не скачался, а не только общий процент.
        var lines = (files || []).map(function (f) {
            var p = state.bundleProgress[bundleFileKey(f)];
            var st = p ? (p.status || 'downloading') : 'pending';
            var pct = p ? Math.round(Number(p.progressPct || 0)) : 0;
            var extra = p && p.status === 'downloading' && p.speedBps ? ' · ' + formatBytes(p.speedBps) + '/s' : '';
            return '<div>' + escapeHtml(f.role + ': ' + f.filename + ' - ' + st + (st === 'downloading' ? ' ' + pct + '%' : '') + extra) + '</div>';
        });
        var statusEl = el('imgBundleStatus');
        if (statusEl) statusEl.innerHTML = lines.join('');
    }

    async function onBundleDownload() {
        var backendId = state.selectedBackendId;
        if (!backendId) { toast(t('image.no_backend_selected', 'Select an image backend first'), 'error'); return; }
        var name = String(val('imgBundleName') || '').trim();
        if (!name) { toast(t('image.bundle_need_name', 'Bundle name is required'), 'error'); return; }
        var family = String(val('imgBundleFamily') || 'other');
        var parsed = parseBundleRows(readBundleRows());
        if (parsed.errors.length) {
            parsed.errors.forEach(function (err) {
                if (err.code === 'need_diffusion') toast(t('image.bundle_need_diffusion', 'A diffusion file is required in the bundle'), 'error');
                else if (err.code === 'missing') toast(t('image.bundle_row_missing_fields', 'Row {row}: repo and file are required', { row: err.row }), 'error');
                else if (err.code === 'dup') toast(t('image.bundle_dup_role', 'Row {row}: duplicate role {role}', { row: err.row, role: err.role }), 'error');
                else toast(t('image.bundle_row_missing_fields', 'Row {row}: repo and file are required', { row: err.row }), 'error');
            });
            return;
        }
        if (!parsed.files.length) { toast(t('image.bundle_need_diffusion', 'A diffusion file is required in the bundle'), 'error'); return; }

        // HF-токен сохраняем до старта: воркер читает его из заголовка запроса.
        var tokenInput = el('imgHfToken');
        if (tokenInput && tokenInput.value) {
            try { localStorage.setItem(HF_TOKEN_KEY, tokenInput.value); } catch (e) { /* ignore */ }
        }

        var body = JSON.stringify({ name: name, family: family, files: parsed.files });
        var started = false;
        var lastErr = null;
        for (var i = 0; i < BUNDLE_DOWNLOAD_PATHS.length && !started; i++) {
            try {
                await requestBackend(backendId, BUNDLE_DOWNLOAD_PATHS[i], { method: 'POST', body: body, timeoutMs: TIMEOUT_CONTROL_MS });
                started = true;
            } catch (e) {
                lastErr = e;
                if (!isMissingEndpoint(e)) break;
            }
        }

        if (!started) {
            if (lastErr && isMissingEndpoint(lastErr)) {
                // Bundle-эндпоинта ещё нет (Phase 2). Деградируем на уже
                // существующий одиночный /api/hf/download по каждому файлу:
                // он принимает role и скачивает файл в каталог моделей воркера.
                toast(t('image.bundle_endpoint_missing', 'Bundle endpoint is not available yet - started per-file downloads instead.'), 'warn');
                var okCount = 0;
                for (var j = 0; j < parsed.files.length; j++) {
                    var f = parsed.files[j];
                    try {
                        await requestBackend(backendId, PATH_HF_DOWNLOAD, {
                            method: 'POST',
                            body: JSON.stringify({ modelId: f.repo, filename: f.filename, revision: f.revision, role: f.role }),
                            timeoutMs: TIMEOUT_CONTROL_MS
                        });
                        okCount++;
                    } catch (e3) {
                        toast(f.role + ': ' + ((e3 && e3.message) || String(e3)), 'error');
                    }
                }
                if (!okCount) return;
            } else {
                toast((lastErr && lastErr.message) || String(lastErr), 'error');
                return;
            }
        }

        toast(t('image.bundle_started', 'Bundle download started'), 'success');
        startBundlePolling(parsed.files);
    }

    /** Прогресс bundle: опрос HFDownloadProgress по каждому файлу. */
    function startBundlePolling(files) {
        stopBundlePolling();
        state.bundleFiles = files;
        state.bundleProgress = {};
        state.bundleIdleTicks = 0;
        var backendId = state.selectedBackendId;
        var tick = function () {
            var tasks = files.map(function (f) {
                var qs = '?modelId=' + encodeURIComponent(f.repo) + '&filename=' + encodeURIComponent(f.filename);
                return requestBackend(backendId, PATH_HF_PROGRESS + qs, { timeoutMs: TIMEOUT_CONTROL_MS })
                    .then(function (p) { state.bundleProgress[bundleFileKey(f)] = p || {}; })
                    .catch(function () { /* 404 = запись ещё не появилась, не ошибка */ });
            });
            Promise.all(tasks).then(function () {
                var agg = aggregateDownloadProgress(files, state.bundleProgress);
                renderBundleProgress(agg, files);
                if (agg.unknown === files.length) {
                    state.bundleIdleTicks++;
                    if (state.bundleIdleTicks >= DOWNLOAD_IDLE_TICKS_MAX) {
                        stopBundlePolling();
                        return;
                    }
                } else {
                    state.bundleIdleTicks = 0;
                }
                if (agg.done) {
                    stopBundlePolling();
                    if (agg.failed) {
                        var failedNames = files.filter(function (f) {
                            var p = state.bundleProgress[bundleFileKey(f)] || {};
                            return p.status === 'failed' || p.status === 'cancelled' || p.status === 'interrupted';
                        }).map(function (f) { return f.filename; }).join(', ');
                        toast(t('image.bundle_failed_files', 'Some files failed: {names}', { names: failedNames }), 'error');
                    } else {
                        toast(t('image.bundle_all_done', 'All bundle files downloaded'), 'success');
                    }
                    // Новые файлы могли уже зарегистрироваться как bundle-профиль
                    // (Phase 2) — перечитываем список моделей, это дешёвый GET.
                    loadModels();
                }
            });
        };
        tick();
        state.bundlePoll = setInterval(tick, DOWNLOAD_POLL_MS);
    }

    function stopBundlePolling() {
        if (state.bundlePoll) clearInterval(state.bundlePoll);
        state.bundlePoll = null;
    }

    // =====================================================================
    // События
    // =====================================================================

    function onClick(e) {
        var target = e.target;
        var actionEl = target && target.closest ? target.closest('[data-img-action]') : null;
        if (!actionEl) return;
        var action = actionEl.getAttribute('data-img-action');
        if (action === 'load-model') {
            onLoadModel(actionEl.getAttribute('data-model'));
        } else if (action === 'unload-model') {
            onUnloadModel(actionEl.getAttribute('data-model'));
        } else if (action === 'bundle-remove') {
            var idx = parseInt(actionEl.getAttribute('data-row'), 10);
            var rows = readBundleRows();
            rows.splice(idx, 1);
            if (!rows.length) rows = [{ role: 'diffusion', repo: '', filename: '' }];
            renderBundleRows(rows);
        } else if (action === 'bundle-files') {
            listBundleFiles(parseInt(actionEl.getAttribute('data-row'), 10) || 0);
        }
    }

    function bindEvents() {
        if (state.bound) return;
        var page = el('image-page');
        if (!page) return;
        state.bound = true;
        // Делегирование: и таблица моделей, и строки bundle перерисовываются
        // через innerHTML, поэтому слушателей на кнопках было бы не навесить.
        page.addEventListener('click', onClick);

        var refreshBtn = el('imgRefreshBtn');
        if (refreshBtn) refreshBtn.addEventListener('click', function () { refresh(); });
        var modelsRefreshBtn = el('imgModelsRefreshBtn');
        if (modelsRefreshBtn) modelsRefreshBtn.addEventListener('click', function () { loadModels(); });
        var backendSel = el('imgBackendSelect');
        if (backendSel) backendSel.addEventListener('change', function () {
            state.selectedBackendId = this.value;
            stopLoadPolling();
            loadModels();
        });
        // #imgModelSelect остался в шапке как индикатор выбранной модели:
        // формы генерации, к которой применялись дефолты профиля, в WebUI больше нет.
        var addRow = el('imgBundleAddRow');
        if (addRow) addRow.addEventListener('click', function () {
            var rows = readBundleRows();
            rows.push({ role: nextSuggestedRole(rows), repo: '', filename: '' });
            renderBundleRows(rows);
        });
        var bundleBtn = el('imgBundleStart');
        if (bundleBtn) bundleBtn.addEventListener('click', onBundleDownload);
        var tokenInput = el('imgHfToken');
        if (tokenInput) tokenInput.addEventListener('change', saveHfToken);
    }

    // =====================================================================
    // Публичный API
    // =====================================================================

    function init() {
        var page = el('image-page');
        if (!page) return;
        bindEvents();
        if (!state.inited) {
            state.inited = true;
            // HF-токен общий с GGUF-страницей — показываем уже сохранённый.
            var tokenInput = el('imgHfToken');
            if (tokenInput) tokenInput.value = hfToken();
            renderFamilySelect();
            renderBundleRows();
            if (window.I18N && typeof I18N.getLang === 'function') {
                // Смена языка перерисовывает статичный HTML через App._updateUITranslations,
                // но динамические куски (таблица моделей, строки bundle) собраны в
                // JS — их нужно перерисовать вручную.
                window.addEventListener('i18n:changed', function () {
                    renderModelsTable();
                    // Строки bundle перерисовываются из текущих значений DOM,
                    // иначе смена языка стёрла бы введённые repo/filename.
                    renderBundleRows(readBundleRows());
                    renderFamilySelect();
                });
            }
        }
        refresh();
        // R-Image Phase 6 (2026-10-02): профили image-моделей - отдельный модуль
        // (image-profiles.js, грузится до этого файла). Хук минимальный: страница
        // владеет только своим DOM, профили - своим контейнером #imageProfiles.
        // Отсутствие модуля не должно ломать страницу (порядок/кэш script-тегов).
        if (window.ImageProfiles && typeof window.ImageProfiles.mount === 'function') {
            try {
                window.ImageProfiles.mount('imageProfiles');
            } catch (e) {
                if (window.console) console.warn('[ImagePage] ImageProfiles.mount failed:', e);
            }
        }
    }

    /** Только данные, без перерисовки статики (кнопка «Обновить» на странице). */
    function refreshData() { return refresh(); }

    window.ImagePage = {
        init: init,
        refresh: refreshData,
        // Экспорт для юнит-тестов (node webui/js/modules/image-page.test.js):
        // чистые функции не зависят от DOM.
        pure: pure,
        _state: state,
        // Действия для интеграционного smoke-теста с мок-DOM
        // (webui/js/modules/image-page-dom.test.js): проверить реальные
        // сценарии (refresh / load / unload / bundle) без браузера.
        _actions: {
            refresh: refresh,
            loadModel: onLoadModel,
            unloadModel: onUnloadModel,
            bundleDownload: onBundleDownload,
            listBundleFiles: listBundleFiles,
            stopPolling: function () { stopLoadPolling(); stopBundlePolling(); }
        }
    };
})();
