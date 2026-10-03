/**
 * image-backends-page.js - страница «Image-бэкенды» (R-Image: управление
 * бэкендами генерации изображений, тип backendType=image_cpp).
 *
 * ЧТО ДЕЛАЕТ:
 *   1. Таблица image-бэкендов кластера: id, имя, хост, порт воркера,
 *      GPU-индекс, состояние воркера, текущая модель, VRAM, счётчики запросов
 *      (total/ok/failed/rejected/in-flight), RPS, среднее время.
 *   2. CRUD бэкенда: POST /api/v1/backends (создать),
 *      PUT /api/v1/backends/{id} (изменить), DELETE (удалить).
 *   3. Управление моделью воркера: POST .../models/load, POST .../models/unload
 *      и GET .../models (список моделей воркера).
 *   4. Видимость пункта навигации и самой страницы: они существуют ТОЛЬКО пока
 *      в кластере есть хотя бы один image_cpp-бэкенд (syncVisibility).
 *
 * ПОЧЕМУ страница отдельная, а не вкладка внутри «Бэкендов»: у image-бэкенда
 * другой набор полей (imagePort/gpuIndex и блок image с моделями и метриками
 * воркера). Общая таблица «Бэкендов» (renderers.js backendsPage) рисует
 * колонки Ollama/llama.cpp и просто не имеет места под состояние sd-воркера.
 *
 * ПОЧЕМУ локальные pure-хелперы, а не Utils: модуль обязан прогоняться в node
 * без DOM (webui/js/modules/image-backends-page.test.js, как image-page.js),
 * а Utils.escapeHtml/formatVramPair/imageStateLabel требуют document или
 * window.I18N. Семантика локальных копий совпадает с utils.js:248-255
 * (formatVramPair) и utils.js:193-201 (imageStateLabel), чтобы колонки
 * «VRAM»/«Состояние» выглядели так же, как в Monitor.
 *
 * ИСТОЧНИК ДАННЫХ: GET /api/v1/cluster (его же опрашивает app.js и передаёт
 * массив в Renderers.backendsPage). У image-бэкенда там есть поле backendType и
 * вложенный блок image {state,currentModel,vramFreeMb,vramTotalMb,requests,models}.
 * В GET /api/v1/backends то же поле типа называется type - поддерживаем оба
 * (element.backendType || element.type), как image-page.js:336.
 *
 * Зависимости: window.I18N, window.Api (createBackend/updateBackend/deleteBackend/
 * post/getAuthHeaders), window.showToast. Экспорт: window.ImageBackendsPage.
 */
(function () {
    'use strict';

    // =====================================================================
    // DOM-контракт (id и классы живут в webui/index.html)
    // =====================================================================
    // Phase 9: страница «Image-модели» (табы) — управление бэкендами теперь её таб
    // «Обзор», отдельного пункта меню нет, поэтому видимость переключаем у
    // объединённой страницы.
    var PAGE_ID = 'image-page';
    var BODY_ID = 'imageBackendsBody';
    var NOTICE_ID = 'imageBackendsNotice';
    // Куда рендерим фактическую политику сосуществования (balancing.image) и как
    // часто её перезапрашивать: страница обновляется на каждом опросе кластера.
    var POLICY_ID = 'imageBackendsPolicy';
    var POLICY_TTL_MS = 15000;
    var OVERLAY_ID = 'ibEditorOverlay';
    var EDITOR_BODY_ID = 'ibEditorBody';

    // Пункт навигации. Основной селектор - как в задании (a[data-page=...]);
    // резервный оставлен на случай смены тега разметки (button вместо a).
    var NAV_SELECTOR = 'a[data-page="image"]';
    var NAV_FALLBACK_SELECTOR = '.nav-item[data-page="image"]';


    // Колонок в таблице: id, имя, хост, порт, GPU, состояние, модель, VRAM,
    // запросы, RPS, среднее время, действия. Нужно для colspan пустых строк.
    var COLUMNS = 12;

    // Сколько вариантов GPU-индекса предлагаем в форме. Индекс - это номер
    // карты в хосте воркера; 8 карт в одной машине уже экзотика, а ввод
    // произвольного числа оставлен свободным (input list/datalist не нужен).
    var GPU_OPTIONS_MAX = 8;

    var TIMEOUT_MS = 15000;

    // Пути воркера (проксируются балансером) - тот же контракт, что у
    // image-page.js:57-59, но здесь управление, а не генерация.
    function pathImageModels(id) { return '/api/v1/image/backends/' + encodeURIComponent(id) + '/models'; }
    function pathImageLoad(id) { return pathImageModels(id) + '/load'; }
    function pathImageUnload(id) { return pathImageModels(id) + '/unload'; }

    /**
     * Типы, которые считаем image-бэкендами. Зеркало image-page.js:351:
     * канон - image_cpp, но в старых конфигах встречается sd_cpp/sdcpp, а в
     * документации/фильтре страницы «Бэкенды» - написание image.cpp
     * (webui/index.html, data-type="image_cpp" плюс человекочитаемое "image.cpp").
     * Голое "image" НЕ принимаем: слишком легко поймать чужой тип.
     */
    var IMAGE_TYPES = ['image_cpp', 'image.cpp', 'sd_cpp', 'sdcpp'];

    // =====================================================================
    // Pure-хелперы (не трогают DOM - покрыты юнит-тестами)
    // =====================================================================

    /** Экранирование HTML: Utils.escapeHtml требует document, а тесты - нет. */
    function escapeHtml(text) {
        if (text === null || text === undefined) return '';
        return String(text)
            .replace(/&/g, '&amp;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;')
            .replace(/'/g, '&#39;');
    }

    function trim(v) {
        return String(v === null || v === undefined ? '' : v).trim();
    }

    /** Число или null (пустая строка/undefined/NaN - это «нет данных», не 0). */
    function toNum(v) {
        if (v === null || v === undefined || v === '') return null;
        var n = Number(v);
        return isFinite(n) ? n : null;
    }

    /** Число или 0 - для счётчиков, где 0 это валидное значение. */
    function num0(v) {
        var n = toNum(v);
        return n === null ? 0 : n;
    }

    /** Список бэкендов из ответа API: массив или {backends:[...]}. */
    function toArray(data) {
        if (Array.isArray(data)) return data;
        if (data && Array.isArray(data.backends)) return data.backends;
        return [];
    }

    /**
     * Тип бэкенда в нижнем регистре. В /api/v1/cluster поле называется
     * backendType, в /api/v1/backends - type; backend_type/engine оставлены
     * как страховка от snake_case-сериализации (как в image-page.js:336).
     */
    function backendTypeOf(b) {
        b = b || {};
        return String(b.backendType || b.backend_type || b.type || b.engine || '').toLowerCase();
    }

    function isImageBackend(b) {
        return IMAGE_TYPES.indexOf(backendTypeOf(b)) !== -1;
    }

    /**
     * gpuIndex -> число или null. ВАЖНО: 0 - валидный индекс первой карты,
     * поэтому проверяем на «есть значение», а не на truthy (иначе GPU 0
     * отображался бы как «не задан»).
     */
    function gpuIndexOf(b) {
        b = b || {};
        var raw = (b.gpuIndex !== undefined && b.gpuIndex !== null) ? b.gpuIndex
            : ((b.gpu_index !== undefined && b.gpu_index !== null) ? b.gpu_index : null);
        if (raw === null || raw === undefined || raw === '') return null;
        var n = toNum(raw);
        return (n === null || n < 0) ? null : n;
    }

    /**
     * Элемент списка бэкендов -> нормализованный вид для таблицы.
     * Блок image может отсутствовать целиком (воркер ещё не отчитался метриками):
     * тогда все поля image пустые, а не «0 запросов» - ноль означал бы
     * «воркер жив и не обработал ни одного запроса», это разные состояния.
     */
    function normalizeBackend(b) {
        b = b || {};
        var img = (b.image && typeof b.image === 'object') ? b.image : {};
        var req = (img.requests && typeof img.requests === 'object') ? img.requests : {};
        var models = Array.isArray(img.models) ? img.models : [];
        return {
            id: trim(b.id || b.ID || ''),
            name: trim(b.name || b.id || ''),
            host: trim(b.host || ''),
            // imagePort - канон (config.Backend.ImagePort, fallback
            // EffectiveImagePort()); image_port - snake_case-страховка.
            port: num0(b.imagePort !== undefined ? b.imagePort : b.image_port),
            gpuIndex: gpuIndexOf(b),
            status: trim(b.status || ''),
            state: trim(img.state || ''),
            currentModel: trim(img.currentModel || img.current_model || ''),
            vramFreeMb: toNum(img.vramFreeMb !== undefined ? img.vramFreeMb : img.vram_free_mb),
            vramTotalMb: toNum(img.vramTotalMb !== undefined ? img.vramTotalMb : img.vram_total_mb),
            lastError: trim(img.lastError || img.last_error || ''),
            models: models.map(function (m) {
                m = m || {};
                return {
                    name: trim(m.name || m.id || ''),
                    state: trim(m.state || ''),
                    family: trim(m.family || ''),
                    vramEstimateMb: num0(m.vramEstimateMb !== undefined ? m.vramEstimateMb : m.vram_estimate_mb)
                };
            }).filter(function (m) { return !!m.name; }),
            requests: {
                total: num0(req.total),
                ok: num0(req.ok),
                failed: num0(req.failed),
                rejected: num0(req.rejected),
                inFlight: num0(req.inFlight !== undefined ? req.inFlight : req.in_flight),
                rps: num0(req.rps),
                avgDurationMs: num0(req.avgDurationMs !== undefined ? req.avgDurationMs : req.avg_duration_ms)
            }
        };
    }

    /** Список бэкендов кластера -> только image-бэкенды, нормализованные. */
    function normalizeBackends(data) {
        return toArray(data).filter(isImageBackend).map(normalizeBackend).filter(function (b) {
            // Бэкенд без id нельзя ни изменить, ни удалить - в таблице ему нечего
            // делать (но на видимость вкладки он всё равно влияет, см.
            // hasImageBackends).
            return !!b.id;
        });
    }

    /** Есть ли в кластере хотя бы один image-бэкенд (для видимости вкладки). */
    function hasImageBackends(data) {
        var arr = toArray(data);
        for (var i = 0; i < arr.length; i++) {
            if (isImageBackend(arr[i])) return true;
        }
        return false;
    }

    /** МБ -> человекочитаемо, как Utils.formatMB (utils.js:19-23). */
    function formatMB(mb) {
        if (mb === null || mb === undefined) return '-';
        if (!isFinite(Number(mb))) return '-';
        var v = Number(mb);
        if (v >= 1024) return (v / 1024).toFixed(1) + ' GB';
        return Math.round(v) + ' MB';
    }

    /**
     * «свободно / всего» для VRAM воркера. Любое отсутствующее значение даёт
     * '-': рисовать «0 MB / 0 MB» нельзя, это читалось бы как «VRAM кончилась»
     * (та же логика, что в Utils.formatVramPair, utils.js:248-255).
     */
    function formatVramPair(freeMb, totalMb) {
        var f = toNum(freeMb);
        var t = toNum(totalMb);
        if (f === null && t === null) return '-';
        return (f === null ? '?' : formatMB(f)) + ' / ' + (t === null ? '?' : formatMB(t));
    }

    /** Индекс GPU: число или локализованное «не задан». */
    function formatGpuIndex(gpu, unknownText) {
        if (gpu === null || gpu === undefined || gpu === '') return unknownText || '-';
        return String(gpu);
    }

    /** RPS: 0.1 -> "0.10", 0 -> "0.00". Округление до сотых - как в мониторе. */
    function formatRps(v) {
        var n = toNum(v);
        if (n === null) return '-';
        return n.toFixed(2);
    }

    /**
     * Среднее время запроса: 950 -> "950 ms", 9100 -> "9.1 s", 125000 -> "2m 05s".
     * Пусто/0 -> '-' (ноль здесь означает «ещё не измеряли», а не «мгновенно»).
     */
    function formatAvgMs(ms) {
        var n = toNum(ms);
        if (n === null || n <= 0) return '-';
        if (n < 1000) return Math.round(n) + ' ms';
        var sec = n / 1000;
        if (sec < 60) return sec.toFixed(1) + ' s';
        var min = Math.floor(sec / 60);
        var rem = Math.round(sec % 60);
        if (min < 60) return min + 'm ' + (rem < 10 ? '0' + rem : rem) + 's';
        var h = Math.floor(min / 60);
        var m = min % 60;
        return h + 'h ' + (m < 10 ? '0' + m : m) + 'm';
    }

    /**
     * Состояние воркера -> ключ i18n. Отдельного набора imageBackends.state_*
     * в паке нет (ключи страницы заморожены), поэтому переиспользуем gguf-ключи
     * состояний модели - ровно так же делает Utils.imageModelStateKey
     * (utils.js:184-188) и image-page.js.
     */
    function stateLabelKey(st) {
        var s = trim(st).toLowerCase();
        if (s === 'loaded') return 'gguf.model_state_loaded';
        if (s === 'loading') return 'gguf.model_state_loading';
        if (s === 'error' || s === 'failed') return 'gguf.model_state_error';
        return 'gguf.model_state_unloaded';
    }

    /**
     * Форма -> ошибки валидации. Возвращаем КОДЫ i18n (не готовый текст):
     * pure-слой не должен зависеть от текущего языка.
     * Порт проверяем строго цифрами: parseInt('18093abc') молча вернул бы
     * 18093, и опечатка ушла бы на сервер как валидный порт.
     */
    function validateForm(form) {
        form = form || {};
        var errors = [];
        if (!trim(form.id)) errors.push({ field: 'id', key: 'imageBackends.err.id_required' });
        if (!trim(form.host)) errors.push({ field: 'host', key: 'imageBackends.err.host_required' });
        var rawPort = trim(form.port);
        var port = /^\d+$/.test(rawPort) ? parseInt(rawPort, 10) : NaN;
        if (isNaN(port) || port < 1 || port > 65535) {
            errors.push({ field: 'port', key: 'imageBackends.err.port_required' });
        }
        return { ok: errors.length === 0, errors: errors };
    }

    /**
     * Значение поля GPU -> число или null.
     * Пусто = «не задан» (для PUT это осознанный сброс в «неизвестно», см.
     * buildUpdatePayload). Нецифровой мусор отдельного ключа ошибки в i18n не
     * имеет, поэтому трактуем его так же, как пустое поле: молча отправить
     * «индекс 0» было бы хуже - оператор увидел бы чужую карту.
     */
    function parseGpuIndex(value) {
        var raw = trim(value);
        if (raw === '') return null;
        if (!/^\d+$/.test(raw)) return null;
        return parseInt(raw, 10);
    }

    /** Тело POST /api/v1/backends (создание image-бэкенда). */
    function buildCreatePayload(form) {
        form = form || {};
        var id = trim(form.id);
        var payload = {
            id: id,
            name: trim(form.name) || id,
            host: trim(form.host),
            imagePort: parseInt(trim(form.port), 10),
            backendType: 'image_cpp'
        };
        // Ключ добавляем ТОЛЬКО когда индекс реально задан: null в POST означал
        // бы «сбросить в неизвестно», а у нового бэкенда сбрасывать нечего
        // (контракт gpuIndex: нет ключа = не менять, null = сброс, число = индекс).
        var gpu = parseGpuIndex(form.gpuIndex);
        if (gpu !== null) payload.gpuIndex = gpu;
        return payload;
    }

    /**
     * Тело PUT /api/v1/backends/{id} (частичное обновление).
     * gpuIndex шлём ВСЕГДА: пустое поле в форме - это явное «сбросить в
     * неизвестно» (null), а не «не трогать». Иначе оператор не смог бы очистить
     * ошибочно указанную карту через UI.
     */
    function buildUpdatePayload(form) {
        form = form || {};
        return {
            name: trim(form.name) || trim(form.id),
            host: trim(form.host),
            imagePort: parseInt(trim(form.port), 10),
            backendType: 'image_cpp',
            gpuIndex: parseGpuIndex(form.gpuIndex)
        };
    }

    /** Список моделей воркера: массив или {models:[...]} -> [{name,state}]. */
    function normalizeModels(data) {
        var arr = Array.isArray(data) ? data : ((data && Array.isArray(data.models)) ? data.models : []);
        return arr.map(function (m) {
            m = m || {};
            return { name: trim(m.name || m.id || ''), state: trim(m.state || '') };
        }).filter(function (m) { return !!m.name; });
    }

    /** Разбор ошибки ответа: {error}, {message} или сырой текст. */
    function parseErrorMessage(body, status) {
        var fallback = 'HTTP ' + (status || '?');
        if (body === null || body === undefined || body === '') return fallback;
        if (typeof body === 'string') return body.length > 400 ? body.slice(0, 400) + '...' : body;
        if (typeof body === 'object') {
            if (typeof body.error === 'string' && body.error) return body.error;
            if (body.error && typeof body.error === 'object' && body.error.message) return String(body.error.message);
            if (body.message) return String(body.message);
            if (body.detail) return String(body.detail);
        }
        return fallback;
    }

    var pure = {
        escapeHtml: escapeHtml,
        backendTypeOf: backendTypeOf,
        isImageBackend: isImageBackend,
        gpuIndexOf: gpuIndexOf,
        normalizeBackend: normalizeBackend,
        normalizeBackends: normalizeBackends,
        hasImageBackends: hasImageBackends,
        normalizeModels: normalizeModels,
        formatMB: formatMB,
        formatVramPair: formatVramPair,
        formatGpuIndex: formatGpuIndex,
        formatRps: formatRps,
        formatAvgMs: formatAvgMs,
        stateLabelKey: stateLabelKey,
        validateForm: validateForm,
        parseGpuIndex: parseGpuIndex,
        buildCreatePayload: buildCreatePayload,
        buildUpdatePayload: buildUpdatePayload,
        parseErrorMessage: parseErrorMessage,
        IMAGE_TYPES: IMAGE_TYPES,
        COLUMNS: COLUMNS
    };

    // =====================================================================
    // Состояние
    // =====================================================================

    var state = {
        backends: [],
        editingId: '',   // '' = форма создания; иначе id редактируемого бэкенда
        bound: false,
        lastError: ''
    };

    // =====================================================================
    // i18n / toast / DOM
    // =====================================================================

    function t(key, fallback, vars) {
        if (window.I18N && typeof window.I18N.t === 'function') {
            var s = window.I18N.t(key, vars);
            if (s !== key) return s;
        }
        var base = (fallback !== undefined) ? fallback : key;
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
        if (window.console) window.console.warn('[ImageBackendsPage]', msg);
    }

    function byId(id) {
        if (!document || typeof document.getElementById !== 'function') return null;
        return document.getElementById(id);
    }

    function setNotice(html, isError) {
        var box = byId(NOTICE_ID);
        if (!box) return;
        if (!html) {
            box.innerHTML = '';
            box.style.display = 'none';
            return;
        }
        box.innerHTML = html;
        box.style.display = '';
        // Инлайн-цвет вместо класса: у notice нет своего класса в CSS, а
        // заводить его в pages.css нельзя (общая таблица стилей вне задачи).
        box.style.color = isError ? 'var(--danger)' : 'var(--text-muted)';
    }

    function errorsText(errors) {
        return (errors || []).map(function (e) { return t(e.key); }).join('; ');
    }

    function queryNav() {
        if (!document || typeof document.querySelector !== 'function') return null;
        return document.querySelector(NAV_SELECTOR) || document.querySelector(NAV_FALLBACK_SELECTOR);
    }

    // =====================================================================
    // Сеть
    // =====================================================================

    function apiBase() {
        var cfg = window.WEBUI_CONFIG || {};
        var base = cfg.API_BASE_URL || cfg.API_BASE ||
            (window.location && window.location.origin) || '';
        return String(base).replace(/\/+$/, '');
    }

    function apiToken() {
        var cfg = window.WEBUI_CONFIG || {};
        if (cfg.API_TOKEN) return cfg.API_TOKEN;
        try { return localStorage.getItem('apiToken') || ''; } catch (e) { return ''; }
    }

    /**
     * Заголовки запроса. База - Api.getAuthHeaders() (api.js:656-662): общий
     * хелпер проекта, чтобы токен и Content-Type не разъезжались между модулями.
     * Fallback на localStorage нужен для dev-режима без entrypoint.sh (там
     * WEBUI_CONFIG.API_TOKEN пуст) - как в image-page.js:609-624.
     */
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

    /**
     * GET к управляющей плоскости. Отдельный локальный хелпер, потому что в
     * api.js нет generic-GET (только перечисленные методы), а тащить в api.js
     * новый метод задача не разрешает: путь /api/v1/image/backends/{id}/models
     * нужен только этой странице.
     */
    function requestJson(path, opts) {
        opts = opts || {};
        var init = { method: opts.method || 'GET', headers: authHeaders(opts.headers) };
        if (opts.body !== undefined) init.body = JSON.stringify(opts.body);
        return fetch(apiBase() + path, init).then(function (resp) {
            return resp.text().then(function (text) {
                var parsed = null;
                if (text) {
                    try { parsed = JSON.parse(text); } catch (e) { parsed = text; }
                }
                if (!resp.ok) {
                    var err = new Error(parseErrorMessage(parsed, resp.status));
                    err.status = resp.status;
                    err.body = parsed;
                    throw err;
                }
                return parsed;
            });
        });
    }

    /** POST/PUT/DELETE через Api, а если модуль Api не подключён - своим fetch. */
    function apiCall(kind, args) {
        var api = window.Api || {};
        if (typeof api[kind] === 'function') return api[kind].apply(api, args);
        if (kind === 'createBackend') return requestJson('/api/v1/backends', { method: 'POST', body: args[0] });
        if (kind === 'updateBackend') return requestJson('/api/v1/backends/' + encodeURIComponent(args[0]), { method: 'PUT', body: args[1] });
        if (kind === 'deleteBackend') return requestJson('/api/v1/backends/' + encodeURIComponent(args[0]), { method: 'DELETE' });
        if (kind === 'post') return requestJson(args[0], { method: 'POST', body: args[1] });
        return Promise.reject(new Error('Api.' + kind + ' is not available'));
    }

    /** Единая обработка ошибки действия: notice + toast, без «тихого» провала. */
    function reportError(prefixKey, prefixFallback, err) {
        var msg = (err && err.message) || String(err);
        var text = t(prefixKey, prefixFallback) + ': ' + msg;
        state.lastError = text;
        setNotice(escapeHtml(text), true);
        toast(text, 'error');
        return text;
    }

    // =====================================================================
    // Рендер таблицы
    // =====================================================================

    /** Кнопка действия строки: data-ib-action - ключ для делегированного клика. */
    function actionButton(action, id, label, cls, disabled) {
        return '<button class="btn ' + cls + ' btn-sm" type="button"' +
            ' data-ib-action="' + escapeHtml(action) + '"' +
            ' data-ib-id="' + escapeHtml(id) + '"' +
            (disabled ? ' disabled' : '') + '>' + escapeHtml(label) + '</button>';
    }

    /** Ячейка счётчиков запросов: значение видно, подпись - в title. */
    function requestsCell(req) {
        function cell(kind, key, value) {
            return '<span data-ib-req="' + kind + '" title="' + escapeHtml(t(key)) + '">' +
                escapeHtml(String(value)) + '</span>';
        }
        return cell('total', 'imageBackends.requests_total', req.total) +
            ' / ' + cell('ok', 'imageBackends.requests_ok', req.ok) +
            ' / ' + cell('failed', 'imageBackends.requests_failed', req.failed) +
            ' / ' + cell('rejected', 'imageBackends.requests_rejected', req.rejected) +
            ' / ' + cell('inflight', 'imageBackends.requests_inflight', req.inFlight);
    }

    function rowHtml(b) {
        var stateText = b.state ? t(stateLabelKey(b.state), b.state) : '-';
        // Во время загрузки модели воркер отвечает не сразу - спиннер честнее
        // статичного «Загружается…» (оператор видит, что UI жив).
        var stateCell = (b.state === 'loading')
            ? '<span title="' + escapeHtml(t('imageBackends.worker_state')) + '">' +
                '<i class="fas fa-circle-notch fa-spin" aria-hidden="true"></i> ' + escapeHtml(stateText) + '</span>'
            : '<span title="' + escapeHtml(t('imageBackends.worker_state')) + '">' + escapeHtml(stateText) + '</span>';

        var modelCell = b.currentModel ? escapeHtml(b.currentModel) : '-';
        if (b.lastError) {
            modelCell += ' <i class="fas fa-triangle-exclamation" style="color:var(--danger);" title="' +
                escapeHtml(b.lastError) + '" aria-hidden="true"></i>';
        }

        // «Выгрузить» доступно только когда есть что выгружать, «Загрузить» -
        // пока воркер не занят загрузкой. Иначе оператор получал бы ошибку
        // воркера вместо понятного состояния кнопки.
        var canUnload = b.state === 'loaded';
        var canLoad = b.state !== 'loading';

        var actions = '<div style="display:flex;gap:6px;flex-wrap:wrap;">' +
            actionButton('edit', b.id, t('imageBackends.edit'), 'btn-secondary') +
            actionButton('remove', b.id, t('imageBackends.remove'), 'btn-danger') +
            // ОДНА кнопка управления моделью, а не две (вторая была бы disabled):
            // у движка одна модель на процесс, поэтому «Загрузить» и «Выгрузить»
            // взаимоисключающи по состоянию. Две кнопки растягивали колонку
            // действий на всю высоту строки и предлагали заведомо недоступное.
            (canUnload
                ? actionButton('unload', b.id, t('imageBackends.unload'), 'btn-secondary')
                : actionButton('load', b.id, t('imageBackends.load'), 'btn-secondary', !canLoad)) +
            actionButton('open-images', b.id, t('imageBackends.open_images'), 'btn-primary') +
            '</div>';

        return '<tr data-ib-id="' + escapeHtml(b.id) + '">' +
            '<td><strong>' + escapeHtml(b.id) + '</strong></td>' +
            '<td>' + escapeHtml(b.name || b.id) + '</td>' +
            '<td>' + (b.host ? escapeHtml(b.host) : '-') + '</td>' +
            '<td>' + (b.port ? escapeHtml(String(b.port)) : '-') + '</td>' +
            '<td>' + escapeHtml(formatGpuIndex(b.gpuIndex, t('imageBackends.gpu_unknown'))) + '</td>' +
            '<td>' + stateCell + '</td>' +
            '<td>' + modelCell + '</td>' +
            '<td>' + escapeHtml(formatVramPair(b.vramFreeMb, b.vramTotalMb)) + '</td>' +
            '<td>' + requestsCell(b.requests) + '</td>' +
            '<td>' + escapeHtml(formatRps(b.requests.rps)) + '</td>' +
            '<td>' + escapeHtml(formatAvgMs(b.requests.avgDurationMs)) + '</td>' +
            '<td>' + actions + '</td>' +
            '</tr>';
    }

    function renderRows() {
        var body = byId(BODY_ID);
        if (!body) return 0;
        if (!state.backends.length) {
            body.innerHTML = '<tr><td colspan="' + COLUMNS + '" class="loading-cell">' +
                escapeHtml(t('imageBackends.empty')) + '</td></tr>';
            return 0;
        }
        body.innerHTML = state.backends.map(rowHtml).join('');
        return state.backends.length;
    }

    /**
     * Отрисовать таблицу по массиву бэкендов (или ответу {backends:[...]}).
     * Возвращает нормализованный список - чтобы вызывающий (app.js/test) видел,
     * что именно попало на страницу.
     */
    function render(backends) {
        state.backends = normalizeBackends(backends);
        renderRows();
        // Политика сосуществования и лимиты гейта - глобальные (balancing.image),
        // а не свойство конкретного бэкенда, поэтому берём их из метрик
        // балансера и обновляем не чаще, чем раз в POLICY_TTL_MS: страница
        // перерисовывается на каждом опросе кластера (раз в несколько секунд).
        loadPolicy(false);
        return state.backends;
    }

    /**
     * Загрузить политику сосуществования и лимиты гейта из /api/v1/metrics.
     *
     * ЗАЧЕМ ИМЕННО ОТТУДА: в /api/v1/cluster (откуда приходят бэкенды) этих
     * полей нет, а /api/v1/metrics отдаёт блок image с уже вычисленными
     * значениями (coexistence_policy, vram_headroom_mb, queue_wait_timeout_sec,
     * exclusive_lock_timeout_sec) - в том числе с учётом дефолтов и env.
     * Оператору на странице управления нужна фактическая политика, а не догадка.
     */
    function loadPolicy(force) {
        var now = Date.now();
        if (!force && state.policyAt && (now - state.policyAt) < POLICY_TTL_MS) return;
        if (state.policyLoading) return;
        state.policyLoading = true;
        requestJson('/api/v1/metrics').then(function (data) {
            state.policyLoading = false;
            state.policyAt = Date.now();
            var img = (data && data.image) || null;
            renderPolicy(img);
        }).catch(function () {
            // Метрики недоступны (нет токена, старый балансер) - не врём
            // прочерком «политики нет», а честно говорим, что не смогли узнать.
            state.policyLoading = false;
            state.policyAt = Date.now();
            var el = byId(POLICY_ID);
            if (el) el.textContent = '—';
        });
    }

    /** Отрисовать политику: exclusive (headroom 512 MB, ожидание 60 s, предохранитель 120 s). */
    function renderPolicy(img) {
        var el = byId(POLICY_ID);
        if (!el) return;
        if (!img || !img.coexistence_policy) {
            el.textContent = '—';
            return;
        }
        var parts = [String(img.coexistence_policy)];
        var headroom = Number(img.vram_headroom_mb || 0);
        if (headroom > 0) parts.push('headroom ' + formatMB(headroom));
        var wait = Number(img.queue_wait_timeout_sec || 0);
        if (wait > 0) parts.push('wait ' + wait + 's');
        var fuse = Number(img.exclusive_lock_timeout_sec || 0);
        if (fuse > 0) parts.push('fuse ' + fuse + 's');
        if (img.gate_disabled) parts.push('gate off');
        el.textContent = parts.filter(function (v) { return !!v; }).join(', ');
    }

    /**
     * Показать/скрыть пункт навигации и страницу. Возвращает true, если
     * image-бэкенды есть (то есть пункт видим).
     */
    function syncVisibility(backends) {
        var visible = hasImageBackends(backends);
        var nav = queryNav();
        if (nav) nav.style.display = visible ? '' : 'none';
        var page = byId(PAGE_ID);
        if (page) page.style.display = visible ? '' : 'none';

        if (!visible) leavePageIfActive(nav);
        return visible;
    }

    /**
     * Последний image-бэкенд исчез, пока оператор стоял на этой странице:
     * инлайн display:none скрыл бы её, но класс .active остался бы, и WebUI
     * показывал бы пустое место вместо контента. Уводим на «Бэкенды» - так же
     * поступает backend-type-filter.js:387-403 при скрытии вкладки Agents.
     */
    function leavePageIfActive(nav) {
        var active = false;
        try {
            active = !!(nav && nav.classList && nav.classList.contains('active'));
        } catch (e) { active = false; }
        if (active) {
            var ctx = window.App && window.App.context;
            if (ctx && typeof ctx.switchPage === 'function') {
                ctx.switchPage('backends');
                return;
            }
            var backendsNav = document.querySelector ? document.querySelector('[data-page="backends"]') : null;
            if (backendsNav && typeof backendsNav.click === 'function') backendsNav.click();
        }
        // Оверлей редактора тоже закрываем: редактировать удалённый бэкенд нельзя.
        closeEditor();
    }

    function findBackend(id) {
        for (var i = 0; i < state.backends.length; i++) {
            if (state.backends[i].id === id) return state.backends[i];
        }
        return null;
    }

    // =====================================================================
    // Редактор (модалка создаётся лениво модулем, как в app.js:2935)
    // =====================================================================

    function editorHtml(b, isNew) {
        b = b || {};
        var gpuOptions = ['<option value="">' + escapeHtml(t('imageBackends.gpu_unknown')) + '</option>'];
        for (var i = 0; i < GPU_OPTIONS_MAX; i++) {
            gpuOptions.push('<option value="' + i + '"' + (b.gpuIndex === i ? ' selected' : '') + '>' + i + '</option>');
        }
        function field(id, labelKey, labelFallback, value, extra) {
            return '<div class="form-group">' +
                '<label for="' + id + '">' + escapeHtml(t(labelKey, labelFallback)) + '</label>' +
                '<input class="form-control" id="' + id + '" type="text" value="' + escapeHtml(value === null || value === undefined ? '' : value) + '"' +
                (extra || '') + '>' +
                '</div>';
        }
        // Тип бэкенда здесь ФИКСИРОВАН (страница только про image_cpp), поэтому
        // повторять его подписью «сосуществование с текстом» нельзя: политика
        // сосуществования задаётся глобально и показывается на самой странице
        // (см. loadPolicy): литерал выглядел как значение настройки.
        return (isNew ? '' : '<p style="margin:0 0 8px;font-size:12px;color:var(--text-muted);">ID: <code>' + escapeHtml(b.id) + '</code></p>') +
            '<div class="form-row">' +
            // ID - ключ бэкенда: при редактировании он не меняется (смена id это
            // создание другого бэкенда + удаление старого, а не PUT).
            field('ibId', 'imageBackends.col_id', 'ID', b.id, isNew ? '' : ' readonly') +
            field('ibName', 'imageBackends.col_name', 'Name', b.name) +
            '</div><div class="form-row">' +
            field('ibHost', 'imageBackends.col_host', 'Host', b.host) +
            field('ibPort', 'imageBackends.col_port', 'Port', b.port || '') +
            '<div class="form-group">' +
            '<label for="ibGpu">' + escapeHtml(t('imageBackends.gpu_index')) + '</label>' +
            '<select class="form-control" id="ibGpu">' + gpuOptions.join('') + '</select>' +
            '</div>' +
            '</div>' +
            '<div id="ibErrors" style="display:none;color:var(--danger);font-size:12px;margin-top:8px;"></div>';
    }

    /** Создать оверлей один раз (идемпотентно). */
    function ensureEditor() {
        var existing = byId(OVERLAY_ID);
        if (existing) return existing;
        if (!document || typeof document.createElement !== 'function' || !document.body) return null;
        var host = document.createElement('div');
        // Классы те же, что у модалки профилей image-моделей
        // (image-profiles.js:1002-1003): .mode-wizard-overlay/.mode-wizard-modal
        // уже описаны в components.css, заводить новые стили не нужно.
        host.innerHTML = '<div class="mode-wizard-overlay" id="ibEditorOverlay" style="display:none;">' +
            '<div class="mode-wizard-modal" role="dialog" aria-modal="true">' +
            '<h3 id="ibEditorTitle"></h3>' +
            '<div id="ibEditorBody"></div>' +
            '<div style="display:flex;gap:8px;justify-content:flex-end;margin-top:12px;">' +
            '<button class="btn btn-secondary" type="button" data-ib-action="cancel">' +
            escapeHtml(t('common.cancel', 'Cancel')) + '</button>' +
            '<button class="btn btn-primary" type="button" data-ib-action="save">' +
            escapeHtml(t('common.save', 'Save')) + '</button>' +
            '</div></div></div>';
        document.body.appendChild(host);
        var overlay = byId(OVERLAY_ID);
        if (overlay && overlay.addEventListener) overlay.addEventListener('click', onEditorClick);
        return overlay;
    }

    function openEditor(backend) {
        var overlay = ensureEditor();
        var body = byId(EDITOR_BODY_ID);
        if (!overlay || !body) return false;
        var isNew = !backend;
        state.editingId = isNew ? '' : trim(backend.id);
        body.innerHTML = editorHtml(backend || {}, isNew);
        var title = byId('ibEditorTitle');
        if (title) title.textContent = isNew ? t('imageBackends.add') : t('imageBackends.edit');
        var errors = byId('ibErrors');
        if (errors) { errors.innerHTML = ''; errors.style.display = 'none'; }
        overlay.style.display = 'flex';
        return true;
    }

    function closeEditor() {
        var overlay = byId(OVERLAY_ID);
        if (overlay) overlay.style.display = 'none';
        state.editingId = '';
    }

    function showEditorErrors(text) {
        var box = byId('ibErrors');
        if (!box) return;
        box.innerHTML = escapeHtml(text);
        box.style.display = '';
    }

    /** Значения полей формы редактора (getElementById - их создаёт editorHtml). */
    function readForm() {
        function value(id) {
            var el = byId(id);
            return el && el.value !== undefined ? el.value : '';
        }
        return {
            id: value('ibId'),
            name: value('ibName'),
            host: value('ibHost'),
            port: value('ibPort'),
            gpuIndex: value('ibGpu')
        };
    }

    function onEditorClick(ev) {
        var btn = closestAction(ev && ev.target);
        if (!btn) return;
        var action = btn.getAttribute('data-ib-action');
        if (action === 'cancel') { closeEditor(); return; }
        if (action === 'save') { saveEditor(); }
    }

    /** Ближайший предок с data-ib-action (closest недоступен в мок-DOM теста). */
    function closestAction(node) {
        var cur = node;
        while (cur) {
            if (cur.getAttribute && cur.getAttribute('data-ib-action')) return cur;
            cur = cur.parentNode;
        }
        return null;
    }

    // =====================================================================
    // Действия
    // =====================================================================

    function refresh() {
        var api = window.Api || {};
        var call = (typeof api.cluster === 'function') ? api.cluster()
            : (typeof api.backends === 'function') ? api.backends()
                : null;
        if (!call) return Promise.resolve([]);
        return Promise.resolve(call).then(function (resp) {
            var list = (resp && resp.backends) ? resp.backends : resp;
            var normalized = render(list);
            syncVisibility(list);
            setNotice('');
            return normalized;
        }).catch(function (e) {
            reportError('common.error', 'Error', e);
            return [];
        });
    }

    function saveEditor() {
        var form = readForm();
        var isNew = !state.editingId;
        var check = validateForm(form);
        if (!check.ok) {
            var text = errorsText(check.errors);
            showEditorErrors(text);
            // Ошибку валидации показываем и в форме, и в notice страницы: форма
            // может быть прокручена, а оператор должен видеть, почему «Сохранить»
            // ничего не сделал (никакого запроса на сервер не уходит).
            setNotice(escapeHtml(text), true);
            toast(text, 'error');
            return Promise.resolve({ ok: false, errors: check.errors });
        }
        var call = isNew
            ? apiCall('createBackend', [buildCreatePayload(form)])
            : apiCall('updateBackend', [state.editingId, buildUpdatePayload(form)]);
        return Promise.resolve(call).then(function () {
            toast(t('imageBackends.saved'), 'success');
            setNotice('');
            closeEditor();
            return refresh().then(function () { return { ok: true }; });
        }).catch(function (e) {
            var text = reportError('common.error', 'Error', e);
            showEditorErrors(text);
            return { ok: false, error: text };
        });
    }

    function onEdit(id) {
        var b = findBackend(id);
        if (!b) return false;
        return openEditor(b);
    }

    function onRemove(id) {
        if (!id) return Promise.resolve(false);
        var question = t('imageBackends.confirm_remove', 'Delete image backend {id}?', { id: id });
        if (typeof window.confirm === 'function' && !window.confirm(question)) {
            return Promise.resolve(false);
        }
        return Promise.resolve(apiCall('deleteBackend', [id])).then(function () {
            toast(t('imageBackends.removed'), 'success');
            setNotice('');
            // Если удалили ту, что открыта в редакторе, - закрываем форму.
            if (state.editingId === id) closeEditor();
            return refresh().then(function () { return true; });
        }).catch(function (e) {
            reportError('common.error', 'Error', e);
            return false;
        });
    }

    /** GET /api/v1/image/backends/{id}/models - список моделей воркера. */
    function listModels(id) {
        return requestJson(pathImageModels(id)).then(normalizeModels).catch(function () {
            // Воркер может не ответить (не поднят/старая сборка) - это не повод
            // падать: выше подставим пустой список и спросим имя у оператора.
            return [];
        });
    }

    /** Имя модели для load/unload: спросить у оператора, если не знаем. */
    function askModelName(suggested) {
        if (typeof window.prompt !== 'function') return '';
        var answer = window.prompt(t('imageBackends.load'), suggested || '');
        return trim(answer);
    }

    function postModelAction(id, kind, model) {
        var path = kind === 'load' ? pathImageLoad(id) : pathImageUnload(id);
        return Promise.resolve(apiCall('post', [path, { name: model }])).then(function () {
            // Ключей вида "модель загружена" в замороженном наборе i18n нет,
            // поэтому тост собирается из названия действия и имени модели -
            // оператору важно видеть, ЧТО именно и на КАКОМ бэкенде сделано.
            toast(t(kind === 'load' ? 'imageBackends.load' : 'imageBackends.unload') + ': ' + model, 'success');
            setNotice('');
            return refresh().then(function () { return true; });
        }).catch(function (e) {
            reportError('common.error', 'Error', e);
            return false;
        });
    }

    function onLoadModel(id) {
        var b = findBackend(id);
        if (!b) return Promise.resolve(false);
        // Текущая модель известна из метрик воркера - самый частый случай:
        // перезагрузить/догрузить то, что уже выбрано.
        if (b.currentModel) return postModelAction(id, 'load', b.currentModel);
        return listModels(id).then(function (models) {
            var picked = askModelName(models.length ? models[0].name : '');
            if (!picked) return false;
            return postModelAction(id, 'load', picked);
        });
    }

    function onUnloadModel(id) {
        var b = findBackend(id);
        if (!b) return Promise.resolve(false);
        if (b.currentModel) return postModelAction(id, 'unload', b.currentModel);
        var picked = askModelName('');
        if (!picked) return Promise.resolve(false);
        return postModelAction(id, 'unload', picked);
    }

    /**
     * «К моделям»: открыть страницу «Image-модели» на табе моделей и предвыбрать
     * этот бэкенд.
     *
     * R-Image Phase 9: раньше кнопка вела на таб генерации, но генерация и показ
     * картинок из WebUI убраны (это задача клиентов) — теперь ведём туда, где с
     * моделью работают: «Модели на диске».
     */
    function openImages(id) {
        var nav = document.querySelector ? document.querySelector('[data-page="image"]') : null;
        if (nav && typeof nav.click === 'function') nav.click();
        if (window.ImageModelsPage && typeof window.ImageModelsPage.showTab === 'function') {
            window.ImageModelsPage.showTab('models');
        }
        // Селект #imgBackendSelect заполняет ImagePage.refresh() асинхронно
        // (GET /api/v1/image/backends), поэтому значение выставляем с
        // повторами: иначе выбор «схлопнется» к первому бэкенду в списке.
        var tries = 0;
        function pick() {
            var sel = byId('imgBackendSelect');
            if (sel && sel.options) {
                for (var i = 0; i < sel.options.length; i++) {
                    if (sel.options[i].value === id) { sel.value = id; return; }
                }
            }
            tries++;
            if (tries < 5 && typeof setTimeout === 'function') setTimeout(pick, 300);
        }
        if (typeof setTimeout === 'function') setTimeout(pick, 150);
        return true;
    }

    /** Делегированный клик по строкам таблицы. */
    function onClick(ev) {
        var btn = closestAction(ev && ev.target);
        if (!btn) return undefined;
        var action = btn.getAttribute('data-ib-action');
        var id = btn.getAttribute('data-ib-id') || '';
        if (action === 'edit') return onEdit(id);
        if (action === 'remove') return onRemove(id);
        if (action === 'load') return onLoadModel(id);
        if (action === 'unload') return onUnloadModel(id);
        if (action === 'open-images') return openImages(id);
        return undefined;
    }

    // =====================================================================
    // Публичный API
    // =====================================================================

    var mountBound = false;

    /** Навесить обработчики (идемпотентно). Возвращает false, если страницы нет. */
    function mount() {
        var page = byId(PAGE_ID);
        if (!page) return false;
        if (mountBound) return true;
        mountBound = true;
        page.addEventListener('click', onClick);
        // R84 (2026-10-03): кнопка «Обновить» удалена из шапки страницы —
        // источник (image-бэкенды) обновляется провайдером активной страницы.
        if (window.DataRefresh && typeof window.DataRefresh.register === 'function') {
            window.DataRefresh.register('image', function () { return refresh(); });
        }
        var addBtn = byId('imageBackendsAdd');
        if (addBtn) addBtn.addEventListener('click', function () { openEditor(null); });
        ensureEditor();
        // Динамические строки таблицы собраны в JS, поэтому смену языка надо
        // обработать вручную (иначе после переключения RU/EN строки останутся
        // на старом языке - App._updateUITranslations правит только статику).
        if (window.addEventListener) {
            window.addEventListener('i18n:changed', function () {
                renderRows();
            });
        }
        return true;
    }

    window.ImageBackendsPage = {
        mount: mount,
        render: render,
        syncVisibility: syncVisibility,
        refresh: refresh,
        // Экспорт для юнит-тестов (node webui/js/modules/image-backends-page.test.js).
        pure: pure,
        _state: state,
        _actions: {
            refresh: refresh,
            save: saveEditor,
            edit: onEdit,
            remove: onRemove,
            loadModel: onLoadModel,
            unloadModel: onUnloadModel,
            listModels: listModels,
            openImages: openImages,
            openEditor: openEditor,
            closeEditor: closeEditor,
            readForm: readForm,
            onClick: onClick,
            editorHtml: editorHtml,
            ensureEditor: ensureEditor
        }
    };
})();
