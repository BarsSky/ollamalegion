/**
 * image-page.js — R-Image Phase 9 (2026-10-03): страница «Image-модели».
 *
 * Что делает (табы «Модели на диске», «Загруженные», «Настройки»):
 *   1. Выбор image-бэкенда и модели в шапке страницы.
 *   2. Управление image-моделями (bundle): список, load / unload, прогресс загрузки.
 *   3. Монтирование редактора профилей (window.ImageProfiles.mount).
 *
 * ЧЕГО ЗДЕСЬ БОЛЬШЕ НЕТ (Phase 9): загрузки bundle с HuggingFace. Она переехала
 * в таб-модуль image-models-hf.js вместе со своими шагами (поиск репозитория →
 * выбор файлов с предложенными ролями → bundle-загрузка → история загрузок).
 * Причина: ручная форма «строка = repo + filename + роль» требовала от оператора
 * знать имена файлов в репозитории заранее, а поля вводились вслепую; стиль
 * страницы «GGUF модели» (поиск → файлы → отметки) эту работу убирает. Держать
 * обе реализации — значит однажды починить одну и забыть другую, поэтому старый
 * код удалён, а не «оставлен на всякий случай».
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
 * window.ImageProfiles (редактор профилей, грузится до этого файла).
 * Экспорт: window.ImagePage.
 */
(function () {
    'use strict';

    // ---- Ключи localStorage ----
    // (HF-токен и его ключ живут теперь в image-models-hf.js: он единственный,
    // кто ходит в /api/hf/*. Ключ там ТОТ ЖЕ — 'ollamalegion_hf_token', общий с
    // GgufApi, чтобы оператор не вводил токен дважды на трёх страницах.)

    // ---- Таймауты и интервалы ----
    var TIMEOUT_CONTROL_MS = 15000;
    var LOAD_POLL_MS = 1500;

    // ---- Пути воркера (контракт плана §5.3, проксируются балансером) ----
    var PATH_MODELS = '/api/image/models';
    var PATH_MODEL_LOAD = '/api/image/models/load';
    var PATH_MODEL_UNLOAD = '/api/image/models/unload';
    var PATH_MODEL_DELETE = '/api/image/models/delete';
    var PATH_LOAD_PROGRESS = '/api/image/models/load/progress';
    // SSE-вариант того же прогресса: приходит push-обновлениями, а не опросом
    // (cmd/sdworker/router.go: /api/image/models/load/progress/stream).
    var PATH_LOAD_PROGRESS_STREAM = '/api/image/models/load/progress/stream';

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

    /**
     * Состав bundle → короткая строка ролей для таблицы («diffusion, vae, clip_l»).
     *
     * ЗАЧЕМ: без состава таблица отвечает «модель есть», но не отвечает на вопрос
     * оператора «а VAE в ней есть?» — а именно из-за отсутствующего VAE sd-server
     * падает при загрузке FLUX/SD3 (pkg/types/image_model.go:260). Роли приходят
     * с сервера (types.ImageModelFile.Role), в JS ничего не выводится из имён.
     */
    function rolesSummary(files) {
        var list = Array.isArray(files) ? files : [];
        var seen = [];
        list.forEach(function (f) {
            var role = String((f && (f.role || f.Role)) || '').trim();
            if (role && seen.indexOf(role) === -1) seen.push(role);
        });
        return seen;
    }

    /** GET /api/image/models → нормализованный список моделей. */
    function normalizeModels(data) {
        var arr = [];
        if (Array.isArray(data)) arr = data;
        else if (data && Array.isArray(data.models)) arr = data.models;
        return arr.map(function (m) {
            m = m || {};
            var d = m.defaults || {};
            var files = Array.isArray(m.files) ? m.files : [];
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
                // Состав bundle: роли файлов + признак применённого профиля.
                roles: rolesSummary(files),
                files_count: files.length,
                bundle_path: m.bundle_path || m.bundlePath || '',
                loaded_at: m.loaded_at || m.loadedAt || '',
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

    /**
     * backendHostCounts — R-MultiHost (2026-10-07): сколько бэкендов объявили
     * каждый host. Имя хоста идентичностью НЕ является: на двух машинах с
     * одинаковым compose контейнеры называются одинаково, и тогда URL одного из
     * бэкендов ведёт на чужую машину.
     */
    function backendHostCounts(backends) {
        var counts = {};
        (backends || []).forEach(function (b) {
            if (b && b.host) counts[b.host] = (counts[b.host] || 0) + 1;
        });
        return counts;
    }

    /**
     * backendOptionLabel — подпись бэкенда в селекте {label, ambiguous}.
     * ambiguous=true, когда такой же host есть ещё у кого-то: в этом случае
     * список моделей и скачивание могут относиться к соседней машине.
     */
    function backendOptionLabel(b, hostCounts) {
        var label = (b && (b.name || b.id)) || '';
        if (b && b.host) {
            label += ' (' + b.host + (b.imagePort ? ':' + b.imagePort : '') + ')';
        }
        var ambiguous = !!(b && b.host && hostCounts && hostCounts[b.host] > 1);
        if (ambiguous) label = '\u26a0 ' + label;
        return { label: label, ambiguous: ambiguous };
    }

    var pure = {
        escapeHtml: escapeHtml,        formatBytes: formatBytes,
        formatDuration: formatDuration,
        parseOpenAIError: parseOpenAIError,
        normalizeModels: normalizeModels,
        normalizeBackends: normalizeBackends,
        rolesSummary: rolesSummary,
        stateLabelKey: stateLabelKey,
        normalizeLoadProgress: normalizeLoadProgress,
        backendHostCounts: backendHostCounts,
        backendOptionLabel: backendOptionLabel
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
        // R88: timeoutMs=0 — осознанное «без капа» (доктрина: работу не рвём);
        // поэтому именно undefined/null, а не `||` (0 || 15000 = 15000).
        var timeoutMs = (opts.timeoutMs === undefined || opts.timeoutMs === null)
            ? TIMEOUT_CONTROL_MS
            : opts.timeoutMs;
        var ctrl = (typeof AbortController !== 'undefined') ? new AbortController() : null;
        var timer = null;
        if (ctrl && timeoutMs > 0) {
            timer = setTimeout(function () { ctrl.abort(); }, timeoutMs);
        }
        var init = { method: opts.method || 'GET', headers: authHeaders(opts.headers) };
        if (ctrl) init.signal = ctrl.signal;
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

    /**
     * Запрос к image-воркеру через прокси балансера.
     *
     * HF-ветки здесь больше нет: /api/hf/* целиком принадлежит
     * image-models-hf.js (вместе с заголовком X-HF-Token). Эта функция ходит
     * только в нативные /api/image/* ручки воркера.
     */
    function requestBackend(backendId, path, opts) {
        opts = opts || {};
        var o = {
            method: opts.method,
            body: opts.body,
            timeoutMs: opts.timeoutMs || TIMEOUT_CONTROL_MS,
            headers: opts.headers
        };
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
        // Сводка параметров бэкенда на табе «Настройки» зависит от выбранного
        // бэкенда и списка бэкендов, а не от моделей — но обновлять её дешевле
        // всего здесь: это единственная точка, где страница узнаёт свежие данные.
        renderBackendParamsState();
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
        // R-MultiHost (2026-10-07): имя хоста - не идентичность. Если у двух
        // бэкендов один host (на двух машинах с одинаковым compose контейнеры
        // называются одинаково), URL одного из них ведёт на чужую машину, и
        // список моделей читается у соседа: «модели на диске есть», хотя у этого
        // воркера их нет. Помечаем такие варианты, чтобы оператор не гадал.
        var hostCounts = backendHostCounts(state.backends);
        var dupWarning = t('image.duplicate_host_warning',
            'Another backend uses the same host - requests for one of them will go to the wrong machine. Set BACKEND_HOST on that machine and recreate its worker.');
        sel.innerHTML = state.backends.map(function (b) {
            var info = backendOptionLabel(b, hostCounts);
            return '<option value="' + escapeHtml(b.id) + '"' +
                (info.ambiguous ? ' title="' + escapeHtml(dupWarning) + '"' : '') +
                (b.id === state.selectedBackendId ? ' selected' : '') + '>' +
                escapeHtml(info.label) + '</option>';
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
        // colspan = число колонок в #imgModelsTable (index.html): 8.
        if (!state.selectedBackendId) {
            body.innerHTML = '<tr><td colspan="8" class="loading-cell">' + escapeHtml(t('image.no_backend_selected', 'Select an image backend first')) + '</td></tr>';
            return;
        }
        if (!state.models.length) {
            body.innerHTML = '<tr><td colspan="8" class="loading-cell">' + escapeHtml(t('image.no_models', 'No image models on this backend.')) + '</td></tr>';
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
                    escapeHtml(t('common.load', 'Load')) + '</button>' +
                    // Удаление с диска — только у НЕзагруженной модели: сервер
                    // всё равно ответит 409 (движок держит веса открытыми), но
                    // кнопка, которая гарантированно даёт ошибку, — плохой UI.
                    ' <button class="btn btn-danger btn-sm" data-img-action="delete-model" data-model="' + escapeHtml(m.name) + '" ' +
                    'title="' + escapeHtml(t('gguf.delete_from_disk', 'Delete downloaded files from disk')) + '">' +
                    '<i class="fas fa-trash"></i></button>';
            }
            var roles = (m.roles && m.roles.length)
                ? m.roles.map(function (r) { return escapeHtml(t('imageModels.role_' + r, r)); }).join(', ')
                : escapeHtml(t('imageModels.roles_unknown', 'нет данных'));
            return '<tr>' +
                '<td><strong>' + escapeHtml(m.name) + '</strong>' + (m.disabled ? ' <span class="badge">disabled</span>' : '') + '</td>' +
                '<td>' + escapeHtml(t(stateLabelKey(m.state), m.state)) + '</td>' +
                '<td>' + escapeHtml(formatBytes(m.size_bytes)) + '</td>' +
                '<td>' + escapeHtml(m.family || '-') + '</td>' +
                '<td style="font-size:12px;">' + roles + '</td>' +
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
            // R88: у запуска загрузки капа нет (0): воркер отвечает 202 сразу, но
            // если запрос пойдёт с wait=true, кап обрезал бы саму загрузку.
            await requestBackend(backendId, PATH_MODEL_LOAD, {
                method: 'POST',
                body: JSON.stringify({ name: name }),
                timeoutMs: 0
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
     * Удаление bundle с диска (Phase 9).
     *
     * ЗАЧЕМ ПОДТВЕРЖДЕНИЕ: это единственная необратимая операция на странице —
     * bundle'ы весят 1.5–12 ГБ, и «случайный клик» означал бы повторное
     * скачивание в сотни мегабайт трафика. confirm() с именем bundle в тексте
     * (ключ imageModels.confirm_delete_bundle) — минимальная защита.
     *
     * Серверная сторона отказывает, если модель загружена (409): показываем её
     * текст как есть, а не своё «не получилось» — там есть подсказка про unload.
     */
    async function onDeleteModel(name) {
        var backendId = state.selectedBackendId;
        if (!backendId || !name) return false;
        var ok = true;
        if (typeof window.confirm === 'function') {
            ok = window.confirm(t('imageModels.confirm_delete_bundle', 'Delete bundle {name} from disk?', { name: name }));
        }
        if (!ok) return false;
        try {
            var res = await requestBackend(backendId, PATH_MODEL_DELETE, {
                method: 'POST',
                body: JSON.stringify({ name: name }),
                timeoutMs: TIMEOUT_CONTROL_MS
            });
            var freed = Number((res && (res.freed_bytes || res.freedBytes)) || 0);
            toast(t('imageModels.deleted_freed', 'Deleted, {mb} MB freed', { mb: Math.round(freed / (1024 * 1024)) }), 'success');
            if (res && res.warning) toast(String(res.warning), 'warn');
        } catch (e) {
            toast(t('gguf.delete_error', 'Failed to delete model file') + ': ' + ((e && e.message) || String(e)), 'error');
            return false;
        }
        // Реестр воркер перечитал сам; перечитываем и список на странице.
        await loadModels();
        return true;
    }

    /**
     * Один снимок прогресса → DOM + терминальные состояния.
     *
     * Общий код для SSE и polling: транспорт отличается, а поведение (показать
     * стадию, закрыть поток на loaded/error, перечитать список моделей) — нет.
     * Иначе две ветки разъехались бы, и «загрузка завершилась, а UI висит» было
     * бы видно только в одном из режимов.
     */
    function applyLoadSnapshot(data, name) {
        var snap = normalizeLoadProgress(data, name);
        if (!snap) {
            // Трекер прогресса уже не помнит запись — считаем загрузку
            // завершённой и перечитываем список моделей (истина о state).
            stopLoadPolling();
            renderLoadProgress(null);
            loadModels();
            return null;
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
        // 'loading'/'starting'/'warming' — ждём дальше.
        return snap;
    }

    /** Периодический опрос прогресса: fallback, если SSE недоступен. */
    function startLoadPollingFallback(name) {
        var backendId = state.selectedBackendId;
        var tick = function () {
            requestBackend(backendId, PATH_LOAD_PROGRESS, { timeoutMs: TIMEOUT_CONTROL_MS }).then(function (data) {
                applyLoadSnapshot(data, name);
            }).catch(function (err) {
                // 404/недоступность прогресса не должны ломать загрузку:
                // останавливаем опрос и просто перечитываем список моделей.
                stopLoadPolling();
                renderLoadProgress(null);
                if (err && !isMissingEndpoint(err)) loadModels();
            });
        };
        tick();
        state.loadPoll = { timer: setInterval(tick, LOAD_POLL_MS), name: name, mode: 'poll' };
    }

    /**
     * Прогресс загрузки модели: SSE, с откатом на polling.
     *
     * ПОЧЕМУ SSE ПЕРВЫМ. Воркер отдаёт поток `data: {json}` на
     * /api/image/models/load/progress/stream (cmd/sdworker/handlers_model.go:342),
     * а прокси балансера стримит `text/event-stream` без буферизации
     * (internal/api/gguf_backend_proxy.go:isSSEResponse/streamCopy) — так же, как
     * для GGUF-страницы (webui/js/modules/gguf-load-progress.js:209). Опрос раз в
     * 1.5 с давал бы задержку до полутора секунд на КАЖДОЙ смене стадии, а
     * загрузка модели — это десятки секунд ожидания, где стадии «spawning» →
     * «loading weights» → «ready» и есть весь фидбек оператору.
     *
     * ПОЧЕМУ С ?token= В URL. EventSource не умеет отправлять заголовки, а
     * прокси-путь закрыт AuthMiddleware — buildProxyUrl дублирует токен в query
     * (AuthMiddleware принимает оба способа).
     *
     * ПОЧЕМУ FALLBACK ОБЯЗАТЕЛЕН. EventSource закрывается сам при HTTP 4xx/5xx
     * (readyState === CLOSED) — например, если воркер старее и ручки нет, или
     * прокси в чужой сборке буферизует поток. Тогда продолжаем опросом: страница
     * не должна остаться без прогресса вообще.
     */
    function startLoadPolling(name) {
        stopLoadPolling();
        var backendId = state.selectedBackendId;
        if (typeof EventSource !== 'function') {
            startLoadPollingFallback(name);
            return;
        }
        var es;
        try {
            es = new EventSource(buildProxyUrl(backendId, PATH_LOAD_PROGRESS_STREAM));
        } catch (e) {
            startLoadPollingFallback(name);
            return;
        }
        state.loadPoll = { es: es, name: name, timer: null, mode: 'sse' };
        es.onmessage = function (ev) {
            if (!state.loadPoll || state.loadPoll.es !== es) return; // поток уже остановлен
            var data = null;
            try { data = JSON.parse(ev.data); } catch (e2) { return; }
            applyLoadSnapshot(data, name);
        };
        es.onerror = function () {
            // CLOSED (2) = переподключения не будет (обычно HTTP 4xx/5xx) —
            // переключаемся на опрос. OPEN/CONNECTING оставляем браузеру:
            // он переподключается сам.
            if (typeof EventSource !== 'undefined' && es.readyState === EventSource.CLOSED) {
                if (!state.loadPoll || state.loadPoll.es !== es) return;
                try { es.close(); } catch (e3) { /* уже закрыт */ }
                state.loadPoll = null;
                startLoadPollingFallback(name);
            }
        };
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
        // Время может быть нулём: воркер старее Phase 9 отдавал elapsed_ms=0, а
        // до старта отсчёта его ещё нет. Тогда НЕ рисуем пустые скобки
        // («Загрузка sd15-q4: ready ()») — это выглядит как сломанный UI, хотя
        // стадия уже получена.
        var elapsed = snap.elapsedMs ? formatDuration(snap.elapsedMs) : '';
        var name = snap.model || '';
        var label;
        if (snap.stage && elapsed) {
            label = t('image.load_progress', 'Loading {name}: {stage} ({t})', { name: name, stage: snap.stage, t: elapsed });
        } else if (snap.stage) {
            label = t('imageModels.load_stage', 'Загрузка {name}: {stage}', { name: name, stage: snap.stage });
        } else if (elapsed) {
            label = t('image.load_progress_plain', 'Loading {name} ({t})', { name: name, t: elapsed });
        } else {
            label = t('imageModels.load_name', 'Загрузка {name}', { name: name });
        }
        setText('imgLoadProgressLabel', label);
        var pct = Number(snap.progressPct);
        var hasPct = !isNaN(pct) && pct > 0;
        show('imgLoadProgressBar', hasPct);
        var fill = el('imgLoadProgressFill');
        if (fill && hasPct) fill.style.width = Math.max(0, Math.min(100, pct)) + '%';
    }
    /** Остановка наблюдения за прогрессом: закрывает и SSE, и polling. */
    function stopLoadPolling() {
        if (state.loadPoll && state.loadPoll.timer) clearInterval(state.loadPoll.timer);
        if (state.loadPoll && state.loadPoll.es) {
            try { state.loadPoll.es.close(); } catch (e) { /* уже закрыт */ }
        }
        state.loadPoll = null;
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
        } else if (action === 'delete-model') {
            onDeleteModel(actionEl.getAttribute('data-model'));
        }
    }

    /**
     * renderBackendParamsState — таб «Настройки»: краткая сводка параметров
     * выбранного бэкенда. Форму держит image-backends-page.js (одна реализация
     * на проект, см. кнопку «Открыть параметры»).
     */
    function renderBackendParamsState() {
        var node = el('imBackendParamsState');
        if (!node) return;
        var b = null;
        (state.backends || []).forEach(function (x) { if (x.id === state.selectedBackendId) b = x; });
        if (!b) { node.textContent = t('imageModels.no_backend_selected', 'Image-бэкенд не выбран'); return; }
        node.textContent = b.name + ' · ' + t('imageBackends.col_host', 'Хост') + ': ' + (b.host || '-') +
            ' · ' + t('imageBackends.col_port', 'Порт воркера') + ': ' + (b.imagePort || '-');
    }

    function bindEvents() {
        if (state.bound) return;
        var page = el('image-page');
        if (!page) return;
        state.bound = true;
        // Делегирование: таблица моделей перерисовывается через innerHTML,
        // поэтому слушателей на кнопках было бы не навесить.
        // (Клики таба HF обрабатывает image-models-hf.js по своим data-imh-action.)
        page.addEventListener('click', onClick);

        // R84 (2026-10-03): своя кнопка «Обновить» удалена — страница
        // регистрирует провайдера, его дёргает общая кнопка/индикатор в шапке.
        // Кнопка у селекта модели остаётся: это отдельный ресурс («перечитать
        // список моделей выбранного бэкенда»), а не общий refresh страницы.
        if (window.DataRefresh && typeof window.DataRefresh.register === 'function') {
            window.DataRefresh.register('image', function () { return refresh(); });
        }
        var modelsRefreshBtn = el('imgModelsRefreshBtn');
        if (modelsRefreshBtn) modelsRefreshBtn.addEventListener('click', function () { loadModels(); });
        var backendSel = el('imgBackendSelect');
        if (backendSel) backendSel.addEventListener('change', function () {
            state.selectedBackendId = this.value;
            stopLoadPolling();
            loadModels();
            renderBackendParamsState();
        });
        // Таб «Настройки»: параметры бэкенда правит его собственный редактор
        // (модалка image-backends-page.js) — так валидация полей одна на проект.
        var paramsBtn = el('imBackendParamsBtn');
        if (paramsBtn) paramsBtn.addEventListener('click', function () {
            var b = null;
            (state.backends || []).forEach(function (x) { if (x.id === state.selectedBackendId) b = x; });
            if (!b) { toast(t('imageModels.no_backend_selected', 'Image-бэкенд не выбран'), 'error'); return; }
            if (window.ImageBackendsPage && window.ImageBackendsPage._actions &&
                typeof window.ImageBackendsPage._actions.openEditor === 'function') {
                window.ImageBackendsPage._actions.openEditor(b);
            } else {
                window.ImageModelsPage && window.ImageModelsPage.showTab &&
                    window.ImageModelsPage.showTab('overview');
            }
        });
        // #imgModelSelect остался в шапке как индикатор выбранной модели:
        // формы генерации, к которой применялись дефолты профиля, в WebUI больше нет.
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
            if (window.I18N && typeof I18N.getLang === 'function') {
                // Смена языка перерисовывает статичный HTML через App._updateUITranslations,
                // но динамические куски (таблица моделей) собраны в JS — их нужно
                // перерисовать вручную. Табы HF/Загрузки перерисовывает их модуль
                // сам, слушая то же событие (image-models-hf.js).
                window.addEventListener('i18n:changed', function () {
                    renderModelsTable();
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
        // сценарии (refresh / load / unload / delete) без браузера. Загрузка
        // bundle проверяется в image-models-hf.test.js — у неё теперь свой модуль.
        _actions: {
            refresh: refresh,
            loadModel: onLoadModel,
            unloadModel: onUnloadModel,
            deleteModel: onDeleteModel,
            renderBackendParamsState: renderBackendParamsState,
            stopPolling: stopLoadPolling
        }
    };
})();
