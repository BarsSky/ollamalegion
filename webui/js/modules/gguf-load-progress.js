/**
 * GgufLoadProgress — модуль real-time прогресса загрузки моделей
 *
 * Round 25 (2026-08-04): переключился с polling на SSE (EventSource).
 *   - cppworker: GET /api/models/load/progress/stream — text/event-stream,
 *     шлёт push-обновления каждые 500ms пока state=loading, потом
 *     финальный event loaded/error и закрывает соединение.
 *   - WebUI: EventSource подписывается на stream, мгновенный feedback
 *     при переходе loading→loaded (0ms vs 1500ms polling).
 *   - Fallback: если EventSource не работает (proxy buffer, network
 *     issue), автоматически переключаемся на старый polling.
 *
 * До Round 25 (polling): каждые 1.5s GET /api/models/load/progress, parse JSON.
 *
 * API (без изменений для обратной совместимости):
 *   GgufLoadProgress.startPolling(backend, isActiveCallback, [opts])
 *     — запускает real-time updates для конкретного бэкенда.
 *     — внутри пытается EventSource (SSE), fallback на polling.
 *     — isActiveCallback() — функция, возвращающая true, пока polling должен
 *       продолжаться (например, return currentBackend() === backend).
 *     — opts: { intervalMs?: number, onUpdate?: (models) => void, onLoaded?: (name)=>void, onError?: (name,err)=>void }
 *
 *   GgufLoadProgress.stopPolling(backendId)
 *     — останавливает updates для бэкенда (закрывает EventSource или clearInterval).
 *
 *   GgufLoadProgress.stopAll()
 *     — останавливает все активные источники.
 *
 *   GgufLoadProgress.isActive(backendId)
 *     — проверяет, активен ли источник для бэкенда.
 *
 *   GgufLoadProgress.getState(backendId)
 *     — возвращает последнее известное состояние моделей: [{name,path,state,elapsedMs,loadingStartedAt,loadingSizeBytes,error}]
 */
(function () {
    'use strict';

    // Map backendId -> { mode: 'sse'|'poll', timer/es, state, backend, isActive, opts, ... }
    const _pollers = Object.create(null);

    /**
     * Создаёт URL для /api/models/load/progress конкретного бэкенда через прокси балансировщика.
     */
    function _buildProgressUrl(backendId) {
        if (window.GgufApi && typeof window.GgufApi.buildBackendProxyUrl === 'function') {
            return window.GgufApi.buildBackendProxyUrl(backendId, '/api/models/load/progress');
        }
        // Fallback: balancer API base
        const balancerUrl = (window.WEBUI_CONFIG && window.WEBUI_CONFIG.API_BASE_URL) ||
            (typeof window !== 'undefined' && window.location && window.location.origin) ||
            '/';
        return balancerUrl.replace(/\/+$/, '') + '/api/v1/gguf/backends/' + encodeURIComponent(backendId) + '/proxy/api/models/load/progress';
    }

    /**
     * Создаёт URL для /api/models/load/progress/stream (SSE endpoint).
     * Round 25: real-time push вместо polling.
     */
    function _buildStreamUrl(backendId) {
        const pollUrl = _buildProgressUrl(backendId);
        // Заменяем последний path segment на /stream.
        // /api/models/load/progress → /api/models/load/progress/stream
        return pollUrl.replace(/\/progress(\?.*)?$/, '/progress/stream$1');
    }

    function _now() {
        return new Date();
    }

    function _formatElapsed(ms) {
        if (!ms || ms < 0) return '0s';
        const sec = Math.floor(ms / 1000);
        if (sec < 60) return sec + 's';
        const min = Math.floor(sec / 60);
        const s = sec % 60;
        return min + 'm ' + s + 's';
    }

    /**
     * Парсит ответ /api/models/load/progress (массив моделей с состоянием loading/error/loaded).
     * Формат cppworker'а: { models: [{name, path, state, loadingStartedAt, loadingSizeBytes, elapsedMs, error}], backend: "..." }
     */
    function _parseProgressResponse(data) {
        let arr = [];
        if (Array.isArray(data)) arr = data;
        else if (data && Array.isArray(data.models)) arr = data.models;
        else if (data && data.model) arr = [data.model];  // single-model запрос
        else if (data && data.name) arr = [data];         // альтернативный формат
        return arr.map(function (m) {
            return {
                name: m.name || m.model || '',
                path: m.path || '',
                state: m.state || (m.error ? 'error' : 'loading'),
                loadingStartedAt: m.loadingStartedAt || m.startedAt || null,
                loadingSizeBytes: m.loadingSizeBytes || m.sizeBytes || 0,
                elapsedMs: m.elapsedMs || 0,
                error: m.error || ''
            };
        });
    }

    /**
     * Нормализует state.loadingModels[backendId] под актуальный ответ сервера.
     * Сохраняет loadingStartedAt, если на сервере оно отсутствует (например, в cached-ответе).
     */
    function _applyToState(backendId, models) {
        const renderer = window.GgufRenderer;
        if (!renderer || typeof renderer.getState !== 'function') return;
        const s = renderer.getState();
        if (!s.loadingModels) s.loadingModels = {};
        const old = s.loadingModels[backendId] || [];
        const oldByName = Object.create(null);
        for (let i = 0; i < old.length; i++) {
            const o = old[i];
            if (o && o.name) oldByName[o.name] = o;
        }
        const next = models.map(function (m) {
            const o = oldByName[m.name] || {};
            return {
                name: m.name,
                path: m.path || o.path || '',
                state: m.state,
                loadingStartedAt: m.loadingStartedAt || o.loadingStartedAt || null,
                loadingSizeBytes: m.loadingSizeBytes || o.loadingSizeBytes || 0,
                elapsedMs: m.elapsedMs || 0,
                error: m.error || ''
            };
        });
        s.loadingModels[backendId] = next;

        // Дёрнем сайдбар renderBackendsList через глобальный updateBackendsList
        if (typeof s._backendsRefreshFn === 'function') {
            try { s._backendsRefreshFn(); } catch (e) { /* noop */ }
        } else if (renderer && typeof renderer.refreshBackends === 'function' && renderer.getState()._lastPolledBackendsRefreshAt) {
            // Лёгкий фоллбек: если есть renderer, можно вызвать refreshBackends (но он может рекурсивно
            // перезагрузить данные; поэтому предпочитаем _backendsRefreshFn, если он зарегистрирован).
        }
    }

    /**
     * Запускает polling для бэкенда.
     * @param {object} backend — backend объект ({id, url, ...})
     * @param {function} isActiveCallback — () => bool
     * @param {object} [opts]
     * @param {number} [opts.intervalMs=1500] — интервал поллинга
     * @param {function} [opts.onUpdate] — (models) => void
     * @param {function} [opts.onLoaded] — (name) => void
     * @param {function} [opts.onError] — (name, errMsg) => void
     * @param {function} [opts.onAllLoaded] — () => void  // вызывается, когда loadingModels[] пустеет
     */
    function startPolling(backend, isActiveCallback, opts) {
        if (!backend || !backend.id) {
            console.warn('[gguf-load-progress] startPolling: backend.id missing');
            return;
        }
        const backendId = backend.id;
        opts = opts || {};
        const intervalMs = opts.intervalMs || 1500;
        const isActive = (typeof isActiveCallback === 'function')
            ? isActiveCallback
            : function () { return true; };

        // Если уже запущен — не дублируем
        if (_pollers[backendId]) {
            // Обновим callback активности (например, при selectBackend сменился)
            _pollers[backendId].isActive = isActive;
            _pollers[backendId].opts = opts;
            return;
        }

        const state = {
            timer: null,
            es: null,           // EventSource instance (SSE mode)
            mode: null,         // 'sse' or 'poll' — set after first attempt
            inFlight: false,
            lastError: null,
            lastActivity: _now(),
            models: [],
            backend: backend,
            isActive: isActive,
            opts: opts
        };
        _pollers[backendId] = state;

        // Round 25: try SSE first, fall back to polling if EventSource fails.
        // EventSource НЕ поддерживается в IE и очень старых браузерах —
        // проверяем typeof и переходим к polling.
        if (typeof EventSource === 'function') {
            _startSSE(backendId, state);
        } else {
            _startPolling(backendId, state);
        }
    }

    /**
     * Запускает EventSource для SSE-обновлений.
     * При ошибке (network, HTTP 4xx/5xx) переключается на polling fallback.
     */
    function _startSSE(backendId, state) {
        const url = _buildStreamUrl(backendId);
        let es;
        try {
            es = new EventSource(url, { withCredentials: true });
        } catch (e) {
            // EventSource constructor может бросить исключение
            // (e.g. invalid URL). Fallback на polling.
            console.debug('[gguf-load-progress] EventSource construct failed, falling back to polling', e);
            _startPolling(backendId, state);
            return;
        }
        state.es = es;
        state.mode = 'sse';

        es.onopen = function () {
            state.lastError = null;
            state.lastActivity = _now();
        };

        es.onmessage = function (event) {
            if (!_pollers[backendId]) return; // был stopPolling
            // Проверяем isActive (пользователь мог уйти с бэкенда).
            if (typeof state.isActive === 'function' && !state.isActive()) {
                stopPolling(backendId);
                return;
            }
            let data;
            try {
                data = JSON.parse(event.data);
            } catch (e) {
                console.debug('[gguf-load-progress] SSE invalid JSON:', event.data, e);
                return;
            }
            state.lastActivity = _now();
            // Single model: data имеет поля name, state, elapsedMs, ...
            // All models: data имеет { models: [...], count, timestamp }.
            const models = _parseProgressResponse(data);
            _applySSEUpdate(backendId, state, models, data);
        };

        es.onerror = function () {
            // EventSource auto-reconnect'ит сам. Но если readyState == CLOSED
            // (= 2), то EventSource решил не переподключаться (часто это
            // значит HTTP 4xx/5xx при initial connect). В этом случае
            // переключаемся на polling.
            if (es.readyState === EventSource.CLOSED) {
                console.debug('[gguf-load-progress] EventSource CLOSED, falling back to polling for', backendId);
                try { es.close(); } catch (e) {}
                if (state.es === es) state.es = null;
                if (state.mode === 'sse') {
                    state.mode = 'poll';
                    _startPolling(backendId, state);
                }
            }
        };
    }

    /**
     * Обрабатывает SSE-update: сохраняет state, вызывает callbacks,
     * детектит terminal state (loaded/error) и закрывает EventSource.
     */
    function _applySSEUpdate(backendId, state, models, rawData) {
        // Terminal state: model loaded/error → закрываем EventSource.
        // Single-model stream: terminal:true в event означает конец.
        if (rawData && rawData.terminal === true) {
            state.models = models;
            _applyToState(backendId, models);
            if (typeof state.opts.onUpdate === 'function') {
                try { state.opts.onUpdate(models); } catch (e) { console.warn('[gguf-load-progress] onUpdate error', e); }
            }
            // Детектим переход loading → loaded / loading → error
            const prev = state._prevModels || [];
            const prevByName = Object.create(null);
            for (let i = 0; i < prev.length; i++) {
                if (prev[i] && prev[i].name) prevByName[prev[i].name] = prev[i];
            }
            for (let i = 0; i < models.length; i++) {
                const m = models[i];
                const p = prevByName[m.name];
                if (p && p.state === 'loading' && m.state === 'loaded') {
                    if (typeof state.opts.onLoaded === 'function') {
                        try { state.opts.onLoaded(m.name); } catch (e) {}
                    }
                } else if (p && p.state === 'loading' && m.state === 'error') {
                    if (typeof state.opts.onError === 'function') {
                        try { state.opts.onError(m.name, m.error || 'unknown'); } catch (e) {}
                    }
                }
            }
            // Terminal: stop streaming. Polling fall back (если есть loading
            // модели на других бэкендах) — это не нужно, т.к. SSE per-backend.
            stopPolling(backendId);
            return;
        }

        // Non-terminal: обычный update.
        state.models = models;
        _applyToState(backendId, models);
        if (typeof state.opts.onUpdate === 'function') {
            try { state.opts.onUpdate(models); } catch (e) { console.warn('[gguf-load-progress] onUpdate error', e); }
        }
        // Детектим переход loading → loaded / loading → error
        const prev = state._prevModels || [];
        const prevByName = Object.create(null);
        for (let i = 0; i < prev.length; i++) {
            if (prev[i] && prev[i].name) prevByName[prev[i].name] = prev[i];
        }
        for (let i = 0; i < models.length; i++) {
            const m = models[i];
            const p = prevByName[m.name];
            if (p && p.state === 'loading' && m.state === 'loaded') {
                if (typeof state.opts.onLoaded === 'function') {
                    try { state.opts.onLoaded(m.name); } catch (e) {}
                }
            } else if (p && p.state === 'loading' && m.state === 'error') {
                if (typeof state.opts.onError === 'function') {
                    try { state.opts.onError(m.name, m.error || 'unknown'); } catch (e) {}
                }
            }
        }
        state._prevModels = models;
        // onAllLoaded: список loading моделей опустел.
        if (models.length === 0 && prev.length > 0) {
            if (typeof state.opts.onAllLoaded === 'function') {
                try { state.opts.onAllLoaded(); } catch (e) {}
            }
        }
    }

    /**
     * Старый polling-based путь (fallback если SSE не работает).
     * Сохранён для обратной совместимости с proxy/балансер без SSE proxying.
     */
    function _startPolling(backendId, state) {
        if (state.mode === 'poll') return; // уже запущен
        state.mode = 'poll';
        const intervalMs = state.opts.intervalMs || 1500;

        const tick = function () {
            if (!_pollers[backendId]) return;          // был stopPolling между тиками
            if (state.inFlight) return;                // запрос ещё в полёте
            if (typeof state.isActive === 'function' && !state.isActive()) {
                // Пользователь ушёл с бэкенда — стоп.
                stopPolling(backendId);
                return;
            }
            state.inFlight = true;
            const url = _buildProgressUrl(backendId);
            const controller = (typeof AbortController === 'function') ? new AbortController() : null;
            const timer = controller ? setTimeout(function () { try { controller.abort(); } catch (e) {} }, 5000) : null;
            fetch(url, {
                method: 'GET',
                headers: { 'Accept': 'application/json' },
                cache: 'no-store',
                ...(controller ? { signal: controller.signal } : {})
            }).then(function (resp) {
                if (timer) clearTimeout(timer);
                if (!resp.ok) {
                    throw new Error('HTTP ' + resp.status);
                }
                return resp.json();
            }).then(function (data) {
                state.inFlight = false;
                state.lastError = null;
                state.lastActivity = _now();
                const models = _parseProgressResponse(data);
                state.models = models;
                _applyToState(backendId, models);
                if (typeof state.opts.onUpdate === 'function') {
                    try { state.opts.onUpdate(models); } catch (e) { console.warn('[gguf-load-progress] onUpdate error', e); }
                }
                // Детектим переход loading → loaded / loading → error
                const prev = state._prevModels || [];
                const prevByName = Object.create(null);
                for (let i = 0; i < prev.length; i++) {
                    if (prev[i] && prev[i].name) prevByName[prev[i].name] = prev[i];
                }
                for (let i = 0; i < models.length; i++) {
                    const m = models[i];
                    const p = prevByName[m.name];
                    if (p && p.state === 'loading' && m.state === 'loaded') {
                        if (typeof state.opts.onLoaded === 'function') {
                            try { state.opts.onLoaded(m.name); } catch (e) { /* noop */ }
                        }
                    } else if (p && p.state === 'loading' && m.state === 'error') {
                        if (typeof state.opts.onError === 'function') {
                            try { state.opts.onError(m.name, m.error || 'unknown'); } catch (e) { /* noop */ }
                        }
                    }
                }
                state._prevModels = models;
                // Если loadingModels[] пуст и раньше были — все загрузились
                if (models.length === 0 && prev.length > 0) {
                    if (typeof state.opts.onAllLoaded === 'function') {
                        try { state.opts.onAllLoaded(); } catch (e) { /* noop */ }
                    }
                    // Polling можно остановить, если нет isActive-callback требования
                    if (typeof state.isActive !== 'function') {
                        stopPolling(backendId);
                    }
                }
            }).catch(function (err) {
                if (timer) clearTimeout(timer);
                state.inFlight = false;
                state.lastError = err && err.message || String(err);
                // На 5xx/network error — не паникуем, polling продолжится на следующем тике.
                if (window.console && console.debug) {
                    console.debug('[gguf-load-progress] poll error for', backendId, state.lastError);
                }
            });
        };

        state.timer = setInterval(tick, intervalMs);
        // Первый тик сразу (не ждём intervalMs)
        setTimeout(tick, 50);
    }

    /**
     * Останавливает polling/SSE для бэкенда.
     * Round 25: закрывает EventSource если активен, иначе clearInterval.
     */
    function stopPolling(backendId) {
        const p = _pollers[backendId];
        if (!p) return;
        if (p.timer) clearInterval(p.timer);
        if (p.es) {
            try { p.es.close(); } catch (e) {}
        }
        delete _pollers[backendId];
    }

    function stopAll() {
        Object.keys(_pollers).forEach(function (id) { stopPolling(id); });
    }

    function isActive(backendId) {
        return !!_pollers[backendId];
    }

    function getState(backendId) {
        const p = _pollers[backendId];
        return p ? p.models.slice() : [];
    }

    function getLastError(backendId) {
        const p = _pollers[backendId];
        return p ? p.lastError : null;
    }

    /**
     * Удобный хелпер: зарегистрировать callback, который renderBackendsList вызывает
     * для триггера ререндера sidebar (используется из gguf-renderer.js).
     */
    function registerBackendsRefreshFn(fn) {
        const renderer = window.GgufRenderer;
        if (!renderer || typeof renderer.getState !== 'function') return;
        renderer.getState()._backendsRefreshFn = fn;
    }

    // Round 25: auto-cleanup on pagehide/visibilitychange.
    // WebUI теряет смысл показывать loading-спиннеры когда вкладка скрыта
    // или браузер закрывается. Останавливаем все активные источники.
    function _autoCleanupOnHide() {
        if (typeof document === 'undefined') return;
        const cleanup = function () {
            if (typeof stopAll === 'function') {
                try { stopAll(); } catch (e) {}
            }
        };
        // pagehide — fired when user navigates away / closes tab (more reliable than unload).
        document.addEventListener('pagehide', cleanup, { capture: true });
        // visibilitychange — cleanup when tab becomes hidden (background).
        document.addEventListener('visibilitychange', function () {
            if (document.visibilityState === 'hidden') {
                cleanup();
            }
        });
    }
    _autoCleanupOnHide();

    window.GgufLoadProgress = {
        startPolling: startPolling,
        stopPolling: stopPolling,
        stopAll: stopAll,
        isActive: isActive,
        getState: getState,
        getLastError: getLastError,
        registerBackendsRefreshFn: registerBackendsRefreshFn,
        // Экспортируем для тестов
        _formatElapsed: _formatElapsed,
        _buildProgressUrl: _buildProgressUrl
    };
})();